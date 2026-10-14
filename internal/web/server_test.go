package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/bus"
	"github.com/babs/grill-room/internal/store"
)

const baseURL = "https://rooms.example"

type env struct {
	ts    *httptest.Server
	st    *store.Store
	bus   *bus.Bus
	srv   *Server
	admin string
	// dbPath is the store's file.
	dbPath string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvOpts(t, testOptions)
}

func newEnvOpts(t *testing.T, opts Options) *env {
	t.Helper()
	return newEnvLog(t, opts, zap.NewNop())
}

// newEnvLog is newEnvOpts with the server writing to log.
func newEnvLog(t *testing.T, opts Options, log *zap.Logger) *env {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "t.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	admin := NewToken()
	b := bus.New(st)
	srv := New(b, HashToken(admin), baseURL+"/", opts, log)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &env{ts: ts, st: st, bus: b, srv: srv, admin: admin, dbPath: dbPath}
}

// do sends a JSON request with an optional bearer token and returns the status and body.
func (e *env) do(t *testing.T, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, e.ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func decodeJSON[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

func (e *env) createRoom(t *testing.T, name string) store.Room {
	t.Helper()
	code, b := e.do(t, "POST", "/api/rooms", e.admin, map[string]string{"name": name})
	if code != http.StatusCreated {
		t.Fatalf("create room: %d %s", code, b)
	}
	return decodeJSON[store.Room](t, b)
}

type invite struct {
	ID    int64  `json:"id"`
	Token string `json:"token"`
	URL   string `json:"url"`
}

func (e *env) createInvite(t *testing.T, room string) invite {
	t.Helper()
	code, b := e.do(t, "POST", "/api/rooms/"+room+"/invites", e.admin, nil)
	if code != http.StatusCreated {
		t.Fatalf("create invite: %d %s", code, b)
	}
	return decodeJSON[invite](t, b)
}

type joined struct {
	Participant store.Participant `json:"participant"`
	Token       string            `json:"token"`
}

func (e *env) join(t *testing.T, inviteToken, name string) joined {
	t.Helper()
	code, b := e.do(t, "POST", "/api/join", "", map[string]string{"invite": inviteToken, "name": name})
	if code != http.StatusCreated {
		t.Fatalf("join: %d %s", code, b)
	}
	return decodeJSON[joined](t, b)
}

type sseEvent struct{ id, event, data string }

// openEvents opens a room's event stream without its "agent" and "presence" events, which their own
// tests read; the channel closes when the server ends the stream.
func (e *env) openEvents(t *testing.T, room, token, lastID string) <-chan sseEvent {
	t.Helper()
	ch, _ := e.openStream(t, "/api/rooms/"+room+"/events", token, lastID)
	out := make(chan sseEvent, 64)
	go func() {
		defer close(out)
		for ev := range ch {
			if ev.event != "agent" && ev.event != "presence" {
				out <- ev
			}
		}
	}()
	return out
}

// openStream opens an SSE stream at path; cancel drops the connection.
func (e *env) openStream(t *testing.T, path, token, lastID string) (<-chan sseEvent, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", e.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // the reader goroutine closes it
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("events: %d", resp.StatusCode)
	}
	ch := make(chan sseEvent, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.data != "" {
					ch <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "id: "):
				ev.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				ev.event = line[7:]
			case strings.HasPrefix(line, "data: "):
				ev.data = line[6:]
			}
		}
	}()
	return ch, cancel
}

func next(t *testing.T, ch <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("stream closed")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5s")
	}
	return sseEvent{}
}

func nextMessage(t *testing.T, ch <-chan sseEvent) store.Message {
	t.Helper()
	ev := next(t, ch)
	if ev.event != "" {
		t.Fatalf("got %q event, want a message", ev.event)
	}
	return decodeJSON[store.Message](t, []byte(ev.data))
}

// expectRevokedThenClosed asserts the stream sends "revoked" and ends.
func expectRevokedThenClosed(t *testing.T, ch <-chan sseEvent) {
	t.Helper()
	// The stream says who hosts first (003-AC26); those events may come before.
	ev := next(t, ch)
	for ev.event == "room" {
		ev = next(t, ch)
	}
	if ev.event != "revoked" {
		t.Fatalf("got %+v, want revoked event", ev)
	}
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("event after revoked: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream still open after revoke")
	}
}

// 001-AC1 001-AC2 001-AC3 001-AC4: the Phase 1 lifecycle end to end over HTTP.
func TestE2ERoomLifecycle(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "Pricing grill")
	inv := e.createInvite(t, room.ID)
	if inv.URL != baseURL+"/join#"+inv.Token {
		t.Fatalf("invite url = %q", inv.URL)
	}

	// 001-AC1: the link opens the join page; a display name is all it takes, no account.
	if code, b := e.do(t, "GET", "/join", "", nil); code != http.StatusOK || !bytes.Contains(b, []byte("join.js")) {
		t.Fatalf("join page: %d", code)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	browser := &http.Client{Jar: jar}
	req, _ := http.NewRequestWithContext(t.Context(), "POST", e.ts.URL+"/api/join",
		strings.NewReader(`{"invite":"`+inv.Token+`","name":"  Alice  "}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	alice := decodeJSON[joined](t, func() []byte { b, _ := io.ReadAll(resp.Body); _ = resp.Body.Close(); return b }())
	if resp.StatusCode != http.StatusCreated || alice.Participant.Name != "Alice" || alice.Participant.RoomID != room.ID {
		t.Fatalf("join: %d %+v", resp.StatusCode, alice)
	}
	c := resp.Cookies()
	if len(c) != 2 || c[0].Path != "/api/rooms/"+room.ID || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie: %+v", c)
	}
	// 002-AC1: the profile cookie is site-wide and long-lived.
	if c[1].Name != profileCookie || c[1].Path != "/" || !c[1].HttpOnly || c[1].SameSite != http.SameSiteStrictMode || c[1].MaxAge != 400*24*3600 {
		t.Fatalf("profile cookie: %+v", c[1])
	}
	// The browser path: the cookie alone authenticates.
	req, _ = http.NewRequestWithContext(t.Context(), "GET", e.ts.URL+"/api/rooms/"+room.ID, nil)
	resp, err = browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	info := decodeJSON[roomResp](t, func() []byte { b, _ := io.ReadAll(resp.Body); _ = resp.Body.Close(); return b }())
	if resp.StatusCode != http.StatusOK || info.Me.Name != "Alice" || info.Room.Name != "Pricing grill" || len(info.Participants) != 1 {
		t.Fatalf("room as Alice: %d %+v", resp.StatusCode, info)
	}

	// Messages: replay of the join line, then live.
	aliceEvents := e.openEvents(t, room.ID, alice.Token, "")
	if m := nextMessage(t, aliceEvents); m.Kind != store.KindSystem || m.Body != "Alice joined" {
		t.Fatalf("replay: %+v", m)
	}
	code, b := e.do(t, "POST", "/api/rooms/"+room.ID+"/messages", alice.Token, map[string]string{"body": "hello\nroom"})
	if code != http.StatusCreated {
		t.Fatalf("post: %d %s", code, b)
	}
	if m := nextMessage(t, aliceEvents); m.Kind != store.KindText || m.Sender != "Alice" || m.Body != "hello\nroom" || m.ParticipantID != alice.Participant.ID {
		t.Fatalf("live message: %+v", m)
	}

	bobInv := e.createInvite(t, room.ID)
	bob := e.join(t, bobInv.Token, "Bob")
	if m := nextMessage(t, aliceEvents); m.Body != "Bob joined" {
		t.Fatalf("join line: %+v", m)
	}
	bobEvents := e.openEvents(t, room.ID, bob.Token, "")
	for range 3 { // Alice joined, hello, Bob joined
		nextMessage(t, bobEvents)
	}

	// 001-AC3: revoking Alice's invite ends her open stream and every later request.
	if code, b := e.do(t, "DELETE", "/api/rooms/"+room.ID+"/invites/"+strconv.FormatInt(inv.ID, 10), e.admin, nil); code != http.StatusOK {
		t.Fatalf("revoke: %d %s", code, b)
	}
	expectRevokedThenClosed(t, aliceEvents)
	for _, r := range []struct{ method, path string }{
		{"GET", "/api/rooms/" + room.ID},
		{"POST", "/api/rooms/" + room.ID + "/messages"},
		{"GET", "/api/rooms/" + room.ID + "/events"},
		{"GET", "/api/rooms/" + room.ID + "/documents"},
		{"POST", "/api/rooms/" + room.ID + "/documents"},
		{"GET", "/api/rooms/" + room.ID + "/documents/1"},
		{"GET", "/api/rooms/" + room.ID + "/documents/1/view"},
		{"GET", "/api/rooms/" + room.ID + "/documents/1/original"},
	} {
		if code, _ := e.do(t, r.method, r.path, alice.Token, map[string]string{"body": "x"}); code != http.StatusUnauthorized {
			t.Errorf("%s %s after revoke: %d, want 401", r.method, r.path, code)
		}
	}
	if code, _ := e.do(t, "POST", "/api/join", "", map[string]string{"invite": inv.Token, "name": "Mallory"}); code != http.StatusUnauthorized {
		t.Errorf("join with revoked invite: %d, want 401", code)
	}
	// Bob joined through another invite and stays in.
	if code, _ := e.do(t, "GET", "/api/rooms/"+room.ID, bob.Token, nil); code != http.StatusOK {
		t.Fatalf("Bob after Alice's revoke: %d", code)
	}

	// 001-AC4: deleting the room ends the remaining streams and leaves nothing behind.
	if code, b := e.do(t, "DELETE", "/api/rooms/"+room.ID, e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("delete room: %d %s", code, b)
	}
	expectRevokedThenClosed(t, bobEvents)
	ctx := t.Context()
	if _, err := e.st.Room(ctx, room.ID); err == nil {
		t.Error("room still readable")
	}
	if msgs, err := e.st.Messages(ctx, room.ID, 0, 100); err != nil || len(msgs) != 0 {
		t.Errorf("messages after delete: %v, %v", msgs, err)
	}
	if ps, err := e.st.Participants(ctx, room.ID); err != nil || len(ps) != 0 {
		t.Errorf("participants after delete: %v, %v", ps, err)
	}
	if code, _ := e.do(t, "GET", "/api/rooms/"+room.ID, bob.Token, nil); code != http.StatusUnauthorized {
		t.Errorf("Bob after delete: %d, want 401", code)
	}
	if code, _ := e.do(t, "POST", "/api/join", "", map[string]string{"invite": bobInv.Token, "name": "Bob"}); code != http.StatusUnauthorized {
		t.Errorf("join a deleted room: %d, want 401", code)
	}
}

// 001-AC2: every room API route and the event stream refuse anything but a valid token for that room.
func TestRoomRoutesRequireValidRoomToken(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R")
	other := e.createRoom(t, "Other")
	stranger := e.join(t, e.createInvite(t, other.ID).Token, "Stranger")
	revInv := e.createInvite(t, room.ID)
	revoked := e.join(t, revInv.Token, "Gone")
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+room.ID+"/invites/"+strconv.FormatInt(revInv.ID, 10), e.admin, nil); code != http.StatusOK {
		t.Fatal("revoke failed")
	}
	member := e.join(t, e.createInvite(t, room.ID).Token, "Member")

	routes := []struct{ method, path string }{
		{"GET", "/api/rooms/" + room.ID},
		{"POST", "/api/rooms/" + room.ID + "/messages"},
		{"GET", "/api/rooms/" + room.ID + "/events"},
		{"GET", "/api/rooms/" + room.ID + "/documents"},
		{"POST", "/api/rooms/" + room.ID + "/documents"},
		{"GET", "/api/rooms/" + room.ID + "/documents/1"},
		{"GET", "/api/rooms/" + room.ID + "/documents/1/view"},
		{"GET", "/api/rooms/" + room.ID + "/documents/1/original"},
	}
	creds := map[string]func(*http.Request){
		"none":              func(*http.Request) {},
		"garbage bearer":    func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") },
		"empty bearer":      func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") },
		"basic scheme":      func(r *http.Request) { r.Header.Set("Authorization", "Basic "+member.Token) },
		"token in query":    func(r *http.Request) { r.URL.RawQuery = "token=" + member.Token },
		"admin token":       func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+e.admin) },
		"other room token":  func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+stranger.Token) },
		"revoked token":     func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+revoked.Token) },
		"garbage cookie":    func(r *http.Request) { r.AddCookie(&http.Cookie{Name: cookieName, Value: "nope"}) },
		"other room cookie": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: cookieName, Value: stranger.Token}) },
	}
	for _, rt := range routes {
		for name, set := range creds {
			t.Run(rt.method+" "+rt.path+" "+name, func(t *testing.T) {
				req, _ := http.NewRequestWithContext(t.Context(), rt.method, e.ts.URL+rt.path, strings.NewReader(`{"body":"x"}`))
				set(req)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("got %d, want 401", resp.StatusCode)
				}
			})
		}
	}
	// The valid token for this room passes on the same routes.
	if code, _ := e.do(t, "GET", "/api/rooms/"+room.ID, member.Token, nil); code != http.StatusOK {
		t.Fatalf("member: %d", code)
	}
}

func TestAdminRoutesRequireAdminToken(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R")
	inv := e.createInvite(t, room.ID)
	member := e.join(t, inv.Token, "Member")
	routes := []struct{ method, path string }{
		{"GET", "/api/rooms"},
		{"POST", "/api/rooms"},
		{"GET", "/api/profiles"},
		{"DELETE", "/api/profiles/1"},
	}
	for _, rt := range routes {
		for _, tok := range []string{"", "nope", member.Token, inv.Token} {
			if code, _ := e.do(t, rt.method, rt.path, tok, map[string]string{"name": "x"}); code != http.StatusUnauthorized {
				t.Errorf("%s %s with %q: %d, want 401", rt.method, rt.path, tok, code)
			}
		}
	}
	if code, _ := e.do(t, "GET", "/api/rooms/"+room.ID, member.Token, nil); code != http.StatusOK {
		t.Fatal("member lost access after failed admin attempts")
	}
}

func TestAdminValidation(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		method, path string
		body         any
		want         int
	}{
		{"POST", "/api/rooms", map[string]string{"name": " "}, http.StatusBadRequest},
		{"POST", "/api/rooms", map[string]string{"name": strings.Repeat("x", maxRoomName+1)}, http.StatusBadRequest},
		{"POST", "/api/rooms", "not an object", http.StatusBadRequest},
		{"POST", "/api/rooms/nope/invites", nil, http.StatusNotFound},
		{"DELETE", "/api/rooms/nope", nil, http.StatusNotFound},
		{"DELETE", "/api/rooms/nope/invites/1", nil, http.StatusNotFound},
		{"DELETE", "/api/rooms/nope/invites/abc", nil, http.StatusNotFound},
	}
	for _, c := range cases {
		if code, b := e.do(t, c.method, c.path, e.admin, c.body); code != c.want {
			t.Errorf("%s %s %v: %d %s, want %d", c.method, c.path, c.body, code, b, c.want)
		}
	}
	room := e.createRoom(t, "R")
	code, b := e.do(t, "GET", "/api/rooms", e.admin, nil)
	if rooms := decodeJSON[[]store.Room](t, b); code != http.StatusOK || len(rooms) != 1 || rooms[0].ID != room.ID {
		t.Fatalf("list rooms: %d %s", code, b)
	}
	// An invite id from another room cannot be revoked through this one.
	other := e.createRoom(t, "Other")
	inv := e.createInvite(t, other.ID)
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+room.ID+"/invites/"+strconv.FormatInt(inv.ID, 10), e.admin, nil); code != http.StatusNotFound {
		t.Fatalf("cross-room revoke: %d, want 404", code)
	}
}

func TestJoinAndMessageValidation(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R")
	inv := e.createInvite(t, room.ID)
	for name, want := range map[string]int{
		"":                      http.StatusBadRequest,
		"   ":                   http.StatusBadRequest,
		"a\x00b":                http.StatusBadRequest,
		"two\nlines":            http.StatusBadRequest,
		strings.Repeat("é", 41): http.StatusBadRequest,
		strings.Repeat("é", 40): http.StatusCreated,
		"Zoë <script>":          http.StatusCreated,
		"evil\u202egnp.exe":     http.StatusBadRequest,
		"\u200fRTL mark":        http.StatusBadRequest,
		"\u2066isolate\u2069":   http.StatusBadRequest,
		"می\u200cخواهم":         http.StatusCreated,
		"👩\u200d💻":              http.StatusCreated,
	} {
		if code, b := e.do(t, "POST", "/api/join", "", map[string]string{"invite": inv.Token, "name": name}); code != want {
			t.Errorf("name %q: %d %s, want %d", name, code, b, want)
		}
	}
	for _, invTok := range []string{"", "nope"} {
		if code, _ := e.do(t, "POST", "/api/join", "", map[string]string{"invite": invTok, "name": "A"}); code != http.StatusUnauthorized {
			t.Errorf("invite %q: %d, want 401", invTok, code)
		}
	}
	if code, _ := e.do(t, "POST", "/api/join", "", "x"); code != http.StatusBadRequest {
		t.Errorf("bad json join: %d", code)
	}
	// A cross-site form can post text/plain or form encodings, never JSON without a CORS preflight.
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", "application/jsonx"} {
		req, _ := http.NewRequestWithContext(t.Context(), "POST", e.ts.URL+"/api/join",
			strings.NewReader(`{"invite":"`+inv.Token+`","name":"CSRF"}`))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Errorf("join with Content-Type %q: %d, want 415", ct, resp.StatusCode)
		}
	}

	p := e.join(t, inv.Token, "A")
	for body, want := range map[string]int{
		"":                                http.StatusBadRequest,
		" \n ":                            http.StatusBadRequest,
		"bell\a":                          http.StatusBadRequest,
		strings.Repeat("x", maxBodyLen+1): http.StatusBadRequest,
		strings.Repeat("x", maxBodyLen):   http.StatusCreated,
		"line one\n\tindented":            http.StatusCreated,
	} {
		if code, _ := e.do(t, "POST", "/api/rooms/"+room.ID+"/messages", p.Token, map[string]string{"body": body}); code != want {
			t.Errorf("body of %d bytes: %d, want %d", len(body), code, want)
		}
	}
	if code, _ := e.do(t, "POST", "/api/rooms/"+room.ID+"/messages", p.Token, strings.Repeat("x", 70<<10)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized request: %d, want 413", code)
	}
}

func TestEventsResumeAfterLastEventID(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R")
	p := e.join(t, e.createInvite(t, room.ID).Token, "A")
	var ids []int64
	for _, body := range []string{"one", "two", "three"} {
		_, b := e.do(t, "POST", "/api/rooms/"+room.ID+"/messages", p.Token, map[string]string{"body": body})
		ids = append(ids, decodeJSON[store.Message](t, b).ID)
	}
	ev := e.openEvents(t, room.ID, p.Token, strconv.FormatInt(ids[1], 10))
	first := next(t, ev)
	if m := decodeJSON[store.Message](t, []byte(first.data)); m.Body != "three" || first.id != strconv.FormatInt(ids[2], 10) {
		t.Fatalf("resume: %+v", first)
	}
}

// 001-AC3: a revocation whose signal never reaches the stream still closes it at the heartbeat.
func TestHeartbeatRechecksAccess(t *testing.T) {
	old := heartbeat
	heartbeat = 50 * time.Millisecond
	t.Cleanup(func() { heartbeat = old })
	e := newEnv(t)
	room := e.createRoom(t, "R")
	inv := e.createInvite(t, room.ID)
	p := e.join(t, inv.Token, "A")
	ev := e.openEvents(t, room.ID, p.Token, "")
	nextMessage(t, ev)
	// Straight to the store: no bus signal.
	if _, err := e.st.RevokeInvite(t.Context(), room.ID, inv.ID); err != nil {
		t.Fatal(err)
	}
	expectRevokedThenClosed(t, ev)
}

// 001-AC5
func TestHealthz(t *testing.T) {
	e := newEnv(t)
	if code, b := e.do(t, "GET", "/healthz", "", nil); code != http.StatusOK || !bytes.Contains(b, []byte(`"ok"`)) {
		t.Fatalf("healthy: %d %s", code, b)
	}
	_ = e.st.Close()
	if code, _ := e.do(t, "GET", "/healthz", "", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("closed store: %d, want 503", code)
	}
}

var iconLink = regexp.MustCompile(`<link rel="(icon|apple-touch-icon|manifest)" href="([^"]+)"`)

// iconTypes maps an icon file extension to the Content-Type browsers need to use it.
var iconTypes = map[string]string{".ico": "image/x-icon", ".svg": "image/svg+xml", ".png": "image/png", ".webmanifest": "application/manifest+json"}

// get fetches path and returns its status, Content-Type and body.
func get(t *testing.T, url string) (int, string, []byte) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), b
}

// Every page links the favicon, a PNG icon, the apple-touch-icon and the manifest; each link and
// each manifest icon is served with the type its extension needs.
func TestIconsAreLinkedAndServed(t *testing.T) {
	e := newEnv(t)
	for _, page := range []string{"/", "/r/abc", "/profile"} {
		_, _, html := get(t, e.ts.URL+page)
		links := iconLink.FindAllStringSubmatch(string(html), -1)
		hrefs := map[string]bool{}
		for _, l := range links {
			hrefs[l[2]] = true
		}
		for _, want := range []string{"/favicon.ico", "/static/icons/icon-32.png", "/static/icons/apple-touch-icon.png", "/site.webmanifest"} {
			if !hrefs[want] {
				t.Errorf("%s does not link %s", page, want)
			}
		}
		for href := range hrefs {
			code, ctype, body := get(t, e.ts.URL+href)
			if code != http.StatusOK || ctype != iconTypes[path.Ext(href)] {
				t.Errorf("%s: %d %q, want 200 %q", href, code, ctype, iconTypes[path.Ext(href)])
			}
			if href != "/site.webmanifest" {
				continue
			}
			var m struct{ Icons []struct{ Src, Type string } }
			if err := json.Unmarshal(body, &m); err != nil || len(m.Icons) < 2 {
				t.Fatalf("manifest: %v %s", err, body)
			}
			for _, ic := range m.Icons {
				if code, ctype, _ := get(t, e.ts.URL+ic.Src); code != http.StatusOK || ctype != ic.Type {
					t.Errorf("manifest icon %s: %d %q, want %q", ic.Src, code, ctype, ic.Type)
				}
			}
		}
	}
}

func TestUIIsServed(t *testing.T) {
	e := newEnv(t)
	for path, want := range map[string]string{
		"/":                  "text/html",
		"/join":              "text/html",
		"/r/abc":             "text/html",
		"/profile":           "text/html",
		"/static/profile.js": "text/javascript",
		"/static/room.js":    "text/javascript",
		"/static/app.css":    "text/css",
		"/static/logo.svg":   "image/svg+xml",
	} {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", e.ts.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if runtime.GOOS == "windows" && strings.HasSuffix(path, ".js") {
			// The registry maps .js to application/javascript; the server ships on Linux.
			want = "application/javascript"
		}
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), want) {
			t.Errorf("%s: %d %s, want %s", path, resp.StatusCode, resp.Header.Get("Content-Type"), want)
		}
		if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
			t.Errorf("%s: no CSP", path)
		}
	}
}

// make icons writes one asset set into both embedded UIs: a copy edited or regenerated alone drifts.
func TestIconCopiesMatch(t *testing.T) {
	pairs := [][2]string{{"ui/logo.svg", "../../design/icons/round2/r2-03-b-three-quarter.svg"}}
	for _, f := range []string{"logo.svg", "favicon.ico", "icons/icon-32.png", "icons/apple-touch-icon.png", "icons/icon-192.png", "icons/icon-512.png"} {
		pairs = append(pairs, [2]string{"ui/" + f, "../daemon/ui/" + f})
	}
	for _, p := range pairs {
		a, err := ui.ReadFile(p[0])
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(p[1])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("%s differs from %s: run make icons", p[0], p[1])
		}
	}
}

// 001-AC4: a room deleted between a stream's access check and its reads still ends the stream
// with "revoked": the reads that fail then must not close it silently. CI once hit this race in
// TestE2ERoomLifecycle; the hook makes it certain.
func TestStreamRevokedWhenRoomGoesMidRead(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "Doomed")
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	events := e.openEvents(t, room.ID, bob.Token, "")
	if m := nextMessage(t, events); m.Body != "Bob joined" {
		t.Fatalf("replay: %+v", m)
	}
	var once sync.Once
	hook := func(string) {
		once.Do(func() {
			if err := e.st.DeleteRoom(context.Background(), room.ID); err != nil {
				t.Error(err)
			}
		})
	}
	afterAccessCheck.Store(&hook)
	t.Cleanup(func() { afterAccessCheck.Store(nil) })
	e.bus.Notify(room.ID)
	expectRevokedThenClosed(t, events)
}
