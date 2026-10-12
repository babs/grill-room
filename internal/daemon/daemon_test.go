package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/bus"
	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/web"
)

func dockerDown(context.Context, io.Reader, ...string) ([]byte, error) {
	return nil, errors.New("Cannot connect to the Docker daemon")
}

func dockerUp(context.Context, io.Reader, ...string) ([]byte, error) {
	return []byte("29.0|Linux"), nil
}

func newTestDaemon(t *testing.T, run sandbox.Runner) *Daemon {
	t.Helper()
	if run == nil {
		run = dockerDown
	}
	dir := t.TempDir()
	d := New(Options{Version: "v-test", Image: "test-image", Tokens: NewTokens(filepath.Join(dir, "state")), Docker: run,
		HTTP: &http.Client{}, Log: zap.NewNop(),
		StatusEvery: 50 * time.Millisecond, PollEvery: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond})
	grant(d, "")
	return d
}

// testBearer stands for the dummy bearer the supervisor hands a worker for an account's proxy.
const testBearer = "sk-ant-oat01-grill-room-dummy-test"

// grant gives room an account proxy, as the supervisor's access message would.
func grant(d *Daemon, room string) {
	d.mu.Lock()
	a := Access{Proxies: maps.Clone(d.access.Proxies)}
	d.mu.Unlock()
	if a.Proxies == nil {
		a.Proxies = map[string]Proxy{}
	}
	a.Proxies[room] = Proxy{Addr: "127.0.0.1:1", Bearer: testBearer}
	d.SetAccess(a)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 001-AC28 003-AC14: hosting is offered only with Docker running and an account proxy for the
// room; the reasons the room sees name neither the machine nor the account (003-AC17).
func TestReadiness(t *testing.T) {
	d := newTestDaemon(t, nil)
	d.Refresh(context.Background())
	s := d.Status()
	if s.CanHost || s.Reason != "Docker is not running: Cannot connect to the Docker daemon" {
		t.Fatalf("docker down: %+v", s)
	}
	d.o.Docker = dockerUp
	d.Refresh(context.Background())
	if s := d.Status(); !s.CanHost || s.Reason != "" || !s.Docker.OK {
		t.Fatalf("ready: %+v", s)
	}
	b, _ := json.Marshal(d.Status())
	if bytes.Contains(b, []byte(testBearer)) {
		t.Fatal("status holds the bearer")
	}
	for _, tc := range []struct {
		access Access
		reason string
	}{
		{Access{}, "the host has no Claude account ready for this room yet"},
		{Access{Proxies: map[string]Proxy{"": {Error: "the host's Claude login has expired"}}}, "the host's Claude login has expired"},
		{Access{Proxies: map[string]Proxy{"": {Addr: "127.0.0.1:1", Bearer: testBearer}}, CutOff: true}, errCutOff.Error()},
		{Access{Proxies: map[string]Proxy{"": {Addr: "127.0.0.1:1", Bearer: testBearer}}}, ""},
	} {
		d.SetAccess(tc.access)
		if s := d.Status(); s.CanHost != (tc.reason == "") || s.Reason != tc.reason {
			t.Errorf("access %+v: status %+v", tc.access, s)
		}
	}
}

type roomServer struct {
	ts    *httptest.Server
	st    *store.Store
	srv   *web.Server
	room  string
	admin string
}

func newRoomServer(t *testing.T) *roomServer {
	t.Helper()
	return newRoomServerOpts(t, web.Options{MaxUploadBytes: 1 << 20, MaxRoomBytes: 1 << 20, MaxAttachmentBytes: 1 << 20})
}

func newRoomServerOpts(t *testing.T, opts web.Options) *roomServer {
	t.Helper()
	return newRoomServerWrapped(t, opts, nil)
}

// newRoomServerWrapped serves the room through wrap, when set.
func newRoomServerWrapped(t *testing.T, opts web.Options, wrap func(http.Handler) http.Handler) *roomServer {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "room.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	admin := web.NewToken()
	ts := httptest.NewUnstartedServer(nil)
	ts.Start()
	srv := web.New(bus.New(st), web.HashToken(admin), ts.URL, opts, zap.NewNop())
	ts.Config.Handler = srv.Handler()
	if wrap != nil {
		ts.Config.Handler = wrap(ts.Config.Handler)
	}
	t.Cleanup(ts.Close)
	rs := &roomServer{ts: ts, st: st, srv: srv, admin: admin}
	var room store.Room
	rs.call(t, "POST", "/api/rooms", admin, map[string]string{"name": "R"}, http.StatusCreated, &room)
	rs.room = room.ID
	return rs
}

func (rs *roomServer) call(t *testing.T, method, path, token string, body any, want int, out any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(t.Context(), method, rs.ts.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatal(err)
		}
	}
}

// join adds a participant and returns its token and invite id.
func (rs *roomServer) join(t *testing.T, name string) (string, int64) {
	t.Helper()
	var inv struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/invites", rs.admin, nil, http.StatusCreated, &inv)
	var j struct {
		Token string `json:"token"`
	}
	rs.call(t, "POST", "/api/join", "", map[string]string{"invite": inv.Token, "name": name}, http.StatusCreated, &j)
	return j.Token, inv.ID
}

type roomDaemon struct {
	ParticipantID int64  `json:"participant_id"`
	CanHost       bool   `json:"can_host"`
	Reason        string `json:"reason"`
}

// hostsFor reports whether participant's own daemon is connected and can host.
func (rs *roomServer) hostsFor(t *testing.T, participant string) bool {
	t.Helper()
	var r struct {
		Me      struct{ ID int64 } `json:"me"`
		Daemons []roomDaemon       `json:"daemons"`
	}
	rs.call(t, "GET", "/api/rooms/"+rs.room, participant, nil, http.StatusOK, &r)
	for _, rd := range r.Daemons {
		if rd.ParticipantID == r.Me.ID && rd.CanHost {
			return true
		}
	}
	return false
}

func (rs *roomServer) daemons(t *testing.T, participant string) []roomDaemon {
	t.Helper()
	var r struct {
		Daemons []roomDaemon `json:"daemons"`
	}
	rs.call(t, "GET", "/api/rooms/"+rs.room, participant, nil, http.StatusOK, &r)
	return r.Daemons
}

// 001-AC22 001-AC28: the daemon pairs through a participant's approval, stores its token without
// showing it, connects outbound, reports that it cannot host while Docker is down, reconnects after
// a dropped connection, and forgets its token once the room revokes it.
func TestPairAndConnect(t *testing.T) {
	rs := newRoomServer(t)
	alice, aliceInvite := rs.join(t, "Alice")
	d := newTestDaemon(t, nil)
	d.Refresh(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	room, err := ParseRoomURL(rs.ts.URL + "/r/" + rs.room)
	if err != nil {
		t.Fatal(err)
	}
	grant(d, room.String())
	d.SetRoom(ctx, room)

	waitFor(t, "pending pairing", func() bool { return d.Status().Pairing == "pending" })
	s := d.Status()
	if !strings.HasPrefix(s.ApproveURL, room.String()+"?pair=") || len(s.Code) != 7 {
		t.Fatalf("status = %+v", s)
	}
	pairID := s.ApproveURL[strings.Index(s.ApproveURL, "=")+1:]
	var shown struct {
		Code string `json:"code"`
	}
	rs.call(t, "GET", "/api/rooms/"+rs.room+"/pairings/"+pairID, alice, nil, http.StatusOK, &shown)
	if shown.Code != s.Code {
		t.Fatalf("room shows code %q, daemon %q", shown.Code, s.Code)
	}
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/pairings/"+pairID, alice, map[string]bool{"approve": true}, http.StatusOK, nil)

	waitFor(t, "connected", func() bool { s := d.Status(); return s.Pairing == "paired" && s.Connection == "connected" })
	tok, err := d.o.Tokens.Get(room)
	if err != nil || tok == "" {
		t.Fatalf("stored token: %q, %v", tok, err)
	}
	if b, _ := json.Marshal(d.Status()); bytes.Contains(b, []byte(tok)) {
		t.Fatal("status shows the daemon token")
	}
	waitFor(t, "room sees the daemon", func() bool {
		ds := rs.daemons(t, alice)
		return len(ds) == 1 && !ds[0].CanHost && strings.HasPrefix(ds[0].Reason, "Docker is not running")
	})
	// Docker comes up: the heartbeat tells the room right away.
	d.o.Docker = dockerUp
	d.Refresh(context.Background())
	waitFor(t, "room sees can_host", func() bool { ds := rs.daemons(t, alice); return len(ds) == 1 && ds[0].CanHost })

	// A dropped connection is retried with backoff.
	rs.ts.CloseClientConnections()
	waitFor(t, "reconnected", func() bool { return d.Status().Connection == "connected" && len(rs.daemons(t, alice)) == 1 })

	// A restarted daemon reuses the stored token: no new pairing.
	cancel()
	waitFor(t, "room drops the daemon", func() bool { return len(rs.daemons(t, alice)) == 0 })
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	d.SetRoom(ctx, room)
	waitFor(t, "connected with the stored token", func() bool { return d.Status().Connection == "connected" })

	rs.call(t, "DELETE", "/api/rooms/"+rs.room+"/invites/"+strconv.FormatInt(aliceInvite, 10), rs.admin, nil, http.StatusNoContent, nil)
	waitFor(t, "revoked", func() bool { s := d.Status(); return s.Pairing == "" && strings.Contains(s.Error, "revoked") })
	if tok, _ := d.o.Tokens.Get(room); tok != "" {
		t.Fatal("revoked token kept")
	}
}

func TestPairingDeniedOrRefused(t *testing.T) {
	rs := newRoomServer(t)
	alice, _ := rs.join(t, "Alice")
	room, _ := ParseRoomURL(rs.ts.URL + "/r/" + rs.room)
	d := newTestDaemon(t, nil)
	d.SetRoom(context.Background(), room)
	waitFor(t, "pending", func() bool { return d.Status().Pairing == "pending" })
	id := d.Status().ApproveURL[strings.Index(d.Status().ApproveURL, "=")+1:]
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/pairings/"+id, alice, map[string]bool{"approve": false}, http.StatusOK, nil)
	waitFor(t, "denied", func() bool { return strings.Contains(d.Status().Error, "denied") })

	d = newTestDaemon(t, nil)
	d.SetRoom(context.Background(), Room{Base: rs.ts.URL, ID: "unknown"})
	waitFor(t, "unknown room error", func() bool { return strings.Contains(d.Status().Error, "no such room") })
}

func TestRunRefreshesUntilCancelled(t *testing.T) {
	d := newTestDaemon(t, dockerUp)
	d.o.CheckEvery = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	waitFor(t, "first refresh", func() bool { return d.Status().CanHost })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// A second room joins the first instead of replacing it: the worker serves both. The page's room
// fields follow the room handed over last, and handing over a room served already keeps its run.
func TestRoomsServedSideBySide(t *testing.T) {
	d := newTestDaemon(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, b := Room{Base: "http://127.0.0.1:1", ID: "a"}, Room{Base: "http://127.0.0.1:1", ID: "b"}
	d.SetRoom(ctx, a)
	d.mu.Lock()
	first := d.rooms[a.String()]
	d.mu.Unlock()
	d.SetRoom(ctx, b)
	if s := d.Status(); len(s.Rooms) != 2 || s.Room != b.String() {
		t.Fatalf("status = %+v", s)
	}
	d.SetRoom(ctx, a)
	d.mu.Lock()
	same := d.rooms[a.String()] == first
	d.mu.Unlock()
	if s := d.Status(); !same || s.Room != a.String() || len(s.Rooms) != 2 {
		t.Fatalf("handing a served room over again restarted it: %v, status %+v", same, s)
	}
	cancel()
	d.Wait()
}
