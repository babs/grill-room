package web

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/babs/grill-room/internal/store"
)

type tokenView struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Scope      string     `json:"scope"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	LastUsedIP string     `json:"last_used_ip"`
	Token      string     `json:"token"`
}

// newToken makes a token from d's browser: scope read or full, days 0 for never.
func (d *device) newToken(t *testing.T, scope string, days int) tokenView {
	t.Helper()
	code, b := d.do(t, "POST", "/api/profile/tokens", map[string]any{"name": "ci " + scope, "scope": scope, "expires_days": days})
	if code != http.StatusCreated {
		t.Fatalf("create token: %d %s", code, b)
	}
	return decodeJSON[tokenView](t, b)
}

func (d *device) tokens(t *testing.T) []tokenView {
	t.Helper()
	code, b := d.do(t, "GET", "/api/profile/tokens", nil)
	if code != http.StatusOK {
		t.Fatalf("list tokens: %d %s", code, b)
	}
	return decodeJSON[struct {
		Tokens []tokenView `json:"tokens"`
	}](t, b).Tokens
}

// errorOf is a refusal's message.
func errorOf(t *testing.T, b []byte) string {
	t.Helper()
	return decodeJSON[map[string]string](t, b)["error"]
}

var tokenRe = regexp.MustCompile(`^grt_[A-Z2-7]{26}$`)

// 012-AC1 012-AC2: the token is answered once; the database holds its hash and nowhere its text,
// and the list never shows either.
func TestAPITokenShownOnceStoredHashed(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	alice.createRoom(t, "R", "Alice")
	tok := alice.newToken(t, "full", 90)
	if !tokenRe.MatchString(tok.Token) || tok.ExpiresAt == nil {
		t.Fatalf("created: %+v", tok)
	}
	db, err := sql.Open("sqlite", e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var hash string
	if err := db.QueryRowContext(t.Context(), `SELECT token_hash FROM api_tokens WHERE id = ?`, tok.ID).Scan(&hash); err != nil || hash != HashToken(tok.Token) {
		t.Fatalf("stored hash: %q %v", hash, err)
	}
	rows, err := db.QueryContext(t.Context(), `SELECT m.name, p.name FROM sqlite_master m JOIN pragma_table_info(m.name) p WHERE m.type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	var cols [][2]string
	for rows.Next() {
		var c [2]string
		if err := rows.Scan(&c[0], &c[1]); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	_ = rows.Close()
	if len(cols) < 50 {
		t.Fatalf("only %d columns scanned", len(cols))
	}
	for _, c := range cols {
		var n int
		q := fmt.Sprintf(`SELECT count(*) FROM %q WHERE CAST(%q AS TEXT) LIKE '%%grt_%%' OR CAST(%q AS TEXT) LIKE ?`, c[0], c[1], c[1])
		if err := db.QueryRowContext(t.Context(), q, "%"+tok.Token[4:]+"%").Scan(&n); err != nil {
			t.Fatalf("%s.%s: %v", c[0], c[1], err)
		}
		if n != 0 {
			t.Errorf("%s.%s holds the token", c[0], c[1])
		}
	}
	code, b := alice.do(t, "GET", "/api/profile/tokens", nil)
	if code != http.StatusOK || strings.Contains(string(b), tok.Token) || strings.Contains(string(b), hash) ||
		strings.Contains(string(b), `"token"`) || strings.Contains(string(b), `"token_hash"`) {
		t.Fatalf("list: %d %s", code, b)
	}
	// Other profiles' browsers see none of them.
	bob := e.device(t)
	bob.join(t, e.createRoom(t, "B").ID, "Bob")
	if ls := bob.tokens(t); len(ls) != 0 {
		t.Fatalf("another profile sees %+v", ls)
	}
	if code, _ := bob.do(t, "DELETE", fmt.Sprintf("/api/profile/tokens/%d", tok.ID), nil); code != http.StatusNotFound {
		t.Fatalf("another profile revokes: %d", code)
	}
	for _, bad := range []map[string]any{
		{"name": "", "scope": "full"},
		{"name": strings.Repeat("x", maxTokenName+1), "scope": "full"},
		{"name": "two\nlines", "scope": "full"},
		{"name": "x", "scope": "admin"},
		{"name": "x", "scope": "read", "expires_days": 7},
	} {
		if code, b := alice.do(t, "POST", "/api/profile/tokens", bad); code != http.StatusBadRequest {
			t.Errorf("%v: %d %s", bad, code, b)
		}
	}
	if code, _ := e.device(t).do(t, "POST", "/api/profile/tokens", map[string]any{"name": "x", "scope": "full"}); code != http.StatusUnauthorized {
		t.Fatalf("without a profile: %d", code)
	}
}

// 012-AC3 012-AC4: a token acts as its profile's participant on the three route shapes, a
// read-only one only reads, and a room the profile is not in is 403.
func TestAPITokenActsAsTheProfile(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Mine", "Alice")
	bob := e.device(t)
	bob.join(t, room.ID, "Bob")
	other := e.createRoom(t, "Other")
	toks := map[string]string{
		"alice full": alice.newToken(t, "full", 30).Token,
		"alice read": alice.newToken(t, "read", 0).Token,
		"bob full":   bob.newToken(t, "full", 30).Token,
	}
	r := "/api/rooms/" + room.ID
	for _, c := range []struct {
		who, method, path string
		body              any
		want              int
		msg               string
	}{
		{"alice read", "GET", r, nil, http.StatusOK, ""},
		{"alice read", "HEAD", r, nil, http.StatusOK, ""},
		{"alice read", "POST", r + "/messages", map[string]string{"body": "hi"}, http.StatusForbidden, msgTokenReadOnly},
		{"alice full", "POST", r + "/messages", map[string]string{"body": "hi"}, http.StatusCreated, ""},
		{"alice read", "GET", r + "/invites", nil, http.StatusOK, ""},
		{"alice read", "PATCH", r, map[string]string{"name": "x"}, http.StatusForbidden, msgTokenReadOnly},
		{"alice full", "PATCH", r, map[string]string{"name": "Renamed"}, http.StatusOK, ""},
		{"bob full", "PATCH", r, map[string]string{"name": "Bob's"}, http.StatusForbidden, "only a room admin can do this"},
		{"alice read", "GET", "/api/profile/skills", nil, http.StatusOK, ""},
		{"alice read", "POST", "/api/profile/rooms", map[string]string{"name": "N", "display_name": "A"}, http.StatusForbidden, msgTokenReadOnly},
		{"alice full", "POST", "/api/profile/rooms", map[string]string{"name": "N", "display_name": "A"}, http.StatusCreated, ""},
		{"alice full", "GET", "/api/rooms/" + other.ID, nil, http.StatusForbidden, msgTokenNotHere},
		{"alice read", "DELETE", "/api/rooms/" + other.ID, nil, http.StatusForbidden, msgTokenReadOnly},
		{"alice full", "GET", "/api/rooms/nope", nil, http.StatusForbidden, msgTokenNotHere},
	} {
		code, b := e.do(t, c.method, c.path, toks[c.who], c.body)
		if code != c.want || (c.msg != "" && errorOf(t, b) != c.msg) {
			t.Errorf("%s %s %s: %d %s, want %d %q", c.who, c.method, c.path, code, b, c.want, c.msg)
		}
	}
	code, b := e.do(t, "GET", "/api/profile", toks["alice read"], nil)
	if p := decodeJSON[profileResp](t, b).Profile; code != http.StatusOK || p == nil || len(p.Rooms) != 3 {
		t.Fatalf("GET /api/profile: %d %s", code, b)
	}
	// The bearer turns cookies off: Bob's browser sending Alice's token acts as Alice.
	code, b = bob.do(t, "GET", r, nil, "Authorization", "Bearer "+toks["alice read"])
	if got := decodeJSON[roomResp](t, b); code != http.StatusOK || got.Me.Name != "Alice" {
		t.Fatalf("token over a cookie: %d %s", code, b)
	}
}

// pathOf fills a route pattern's wildcards: {room} with room, the rest with 1.
func pathOf(pattern, room string) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	path = strings.ReplaceAll(path, "{$}", "")
	path = strings.ReplaceAll(path, "{room}", room)
	return method, regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(path, "1")
}

// 012-AC5: every admin route refuses a token, whatever its scope: the test walks the routes the
// server registered, so a route added later is covered. Admin-only routes are the ones that ask
// an anonymous caller for the admin token.
func TestAPITokenNeverReachesAdmin(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Mine", "Alice")
	toks := []string{alice.newToken(t, "full", 0).Token, alice.newToken(t, "read", 0).Token}
	admin := 0
	for _, pattern := range e.srv.routes {
		method, path := pathOf(pattern, room.ID)
		if !strings.HasPrefix(path, "/api/") {
			continue
		}
		code, b := e.do(t, method, path, "", nil)
		if !strings.HasPrefix(path, "/api/admin/") && (code != http.StatusUnauthorized || !strings.Contains(string(b), "admin token or session required")) {
			continue
		}
		admin++
		for _, tok := range toks {
			if code, b := e.do(t, method, path, tok, map[string]string{}); code != http.StatusForbidden || errorOf(t, b) != msgTokenAdmin {
				t.Errorf("%s with a token: %d %s", pattern, code, b)
			}
		}
	}
	if admin < 40 {
		t.Fatalf("only %d admin routes found", admin)
	}
}

// 012-AC6: the routes that make a credential stay cookie only; each is a registered route.
func TestAPITokenCookieOnlyRoutes(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Mine", "Alice")
	tok := alice.newToken(t, "full", 0).Token
	registered := map[string]bool{}
	for _, p := range e.srv.routes {
		registered[p] = true
	}
	// The routes AC6 names, written out here: dropping one from cookieOnly fails this test.
	for _, pattern := range []string{
		"GET /api/profile/tokens", "POST /api/profile/tokens", "DELETE /api/profile/tokens/{id}", "POST /api/join",
		"POST /api/profile/email", "POST /api/profile/signin", "POST /api/profile/magic", "POST /api/profile/recover",
		"POST /api/rooms/{room}/pairings", "GET /api/pairings/{id}", "GET /api/rooms/{room}/pairings/{id}", "POST /api/rooms/{room}/pairings/{id}",
		"GET /api/rooms/{room}/push", "PUT /api/rooms/{room}/push", "DELETE /api/rooms/{room}/push",
	} {
		if !registered[pattern] {
			t.Errorf("cookieOnly names %q, which no route registers", pattern)
			continue
		}
		method, path := pathOf(pattern, room.ID)
		if code, b := e.do(t, method, path, tok, map[string]string{}); code != http.StatusForbidden || errorOf(t, b) != msgTokenCookie {
			t.Errorf("%s: %d %s", pattern, code, b)
		}
	}
	// The shareable invite is the exception (Q2).
	if code, b := e.do(t, "POST", "/api/rooms/"+room.ID+"/invites/share", tok, nil); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("share invite: %d %s", code, b)
	}
}

// 012-AC7 012-AC8 012-AC9 012-AC11: expiry, revoke, the throttled last use and its IP, and the cap.
func TestAPITokenLifecycle(t *testing.T) {
	e := newEnv(t)
	clk := e.clock()
	alice := e.device(t)
	alice.createRoom(t, "Mine", "Alice")
	short := alice.newToken(t, "read", 30)
	use := func(tok string, xff string) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), "GET", e.ts.URL+"/api/profile", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		msg, _ := body["error"].(string)
		return resp.StatusCode, msg
	}
	lastUse := func() tokenView {
		t.Helper()
		for _, v := range alice.tokens(t) {
			if v.ID == short.ID {
				return v
			}
		}
		t.Fatal("token gone")
		return tokenView{}
	}
	if v := lastUse(); v.LastUsedAt != nil {
		t.Fatalf("never used: %+v", v)
	}
	start := clk.now()
	if code, _ := use(short.Token, "192.0.2.7"); code != http.StatusOK {
		t.Fatalf("first use: %d", code)
	}
	if v := lastUse(); v.LastUsedAt == nil || !v.LastUsedAt.Equal(start) || v.LastUsedIP != "192.0.2.7" {
		t.Fatalf("after one use: %+v", v)
	}
	clk.add(10 * time.Second)
	use(short.Token, "192.0.2.8")
	if v := lastUse(); !v.LastUsedAt.Equal(start) || v.LastUsedIP != "192.0.2.7" {
		t.Fatalf("10 s later, written again: %+v", v)
	}
	clk.add(51 * time.Second)
	use(short.Token, "192.0.2.8")
	if v := lastUse(); !v.LastUsedAt.Equal(start.Add(61*time.Second)) || v.LastUsedIP != "192.0.2.8" {
		t.Fatalf("61 s later: %+v", v)
	}

	for range store.MaxAPITokens - 1 {
		alice.newToken(t, "full", 0)
	}
	code, b := alice.do(t, "POST", "/api/profile/tokens", map[string]any{"name": "one more", "scope": "full"})
	if code != http.StatusConflict || errorOf(t, b) != "You have 100 API tokens. Revoke one first." {
		t.Fatalf("101st: %d %s", code, b)
	}
	clk.add(31 * 24 * time.Hour)
	if code, msg := use(short.Token, ""); code != http.StatusUnauthorized || msg != msgTokenExpired {
		t.Fatalf("expired: %d %q", code, msg)
	}
	if v := lastUse(); v.ExpiresAt == nil || !v.ExpiresAt.Before(clk.now()) {
		t.Fatalf("an expired token stays listed: %+v", v)
	}
	live := alice.newToken(t, "read", 0)
	if code, _ := use(live.Token, ""); code != http.StatusOK {
		t.Fatalf("live: %d", code)
	}
	if code, b := alice.do(t, "DELETE", fmt.Sprintf("/api/profile/tokens/%d", live.ID), nil); code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", code, b)
	}
	if code, msg := use(live.Token, ""); code != http.StatusUnauthorized || msg != "invalid or revoked token" {
		t.Fatalf("revoked: %d %q", code, msg)
	}
}

// 012-AC10: create and revoke are audited with the token's details; an audited action done with a
// token names it; no log line holds a token.
func TestAPITokenAuditAndLogs(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	e := newEnvLog(t, testOptions, zap.New(core))
	alice := e.device(t)
	room := alice.createRoom(t, "Mine", "Alice")
	tok := alice.newToken(t, "full", 365)
	if code, b := e.do(t, "PATCH", "/api/rooms/"+room.ID, tok.Token, map[string]string{"name": "Renamed"}); code != http.StatusOK {
		t.Fatalf("rename: %d %s", code, b)
	}
	if code, _ := e.do(t, "GET", "/api/rooms/"+room.ID, "grt_WRONGWRONGWRONGWRONGWRONG", nil); code != http.StatusUnauthorized {
		t.Fatalf("unknown token: %d", code)
	}
	if code, _ := alice.do(t, "DELETE", fmt.Sprintf("/api/profile/tokens/%d", tok.ID), nil); code != http.StatusNoContent {
		t.Fatal("revoke")
	}
	got := e.auditLog(t, "").Entries
	if len(got) < 3 || got[0].Action != ActionAPITokenRevoke || got[1].Action != ActionRoomRename || got[2].Action != ActionAPITokenCreate {
		t.Fatalf("audit: %+v", got)
	}
	var create, rename map[string]any
	_ = json.Unmarshal(got[2].Details, &create)
	_ = json.Unmarshal(got[1].Details, &rename)
	if !strings.HasPrefix(got[2].Actor, ActorProfile) || got[2].TargetKind != store.TargetProfile || got[2].TargetName != "Alice" ||
		create["token_id"] != float64(tok.ID) || create["name"] != "ci full" || create["scope"] != "full" || create["expires_at"] == nil {
		t.Fatalf("create entry: %+v %s", got[2], got[2].Details)
	}
	if rename["api_token"] != float64(tok.ID) {
		t.Fatalf("rename entry: %s", got[1].Details)
	}
	for _, en := range got {
		if strings.Contains(string(en.Details), "grt_") || strings.Contains(string(en.Details), HashToken(tok.Token)) {
			t.Fatalf("audit holds the token: %s", en.Details)
		}
	}
	for _, l := range logs.All() {
		b, _ := json.Marshal(l.ContextMap())
		if strings.Contains(l.Message+string(b), "grt_") {
			t.Fatalf("log line holds a token: %s %s", l.Message, b)
		}
	}
}
