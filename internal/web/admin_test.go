package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// clock is a settable time for the server's admin sessions, login limits and audit log.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (e *env) clock() *clock {
	c := &clock{t: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	e.srv.now = c.now
	return c
}

// login signs d in to /admin with token from client IP ip, and returns the status and the cookie set.
func (d *device) login(t *testing.T, token, ip string) (int, *http.Cookie) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), "POST", d.e.ts.URL+"/api/admin/login", strings.NewReader(`{"token":"`+token+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	req.Header.Set("User-Agent", "test-browser")
	resp, err := d.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	for _, c := range resp.Cookies() {
		if c.Name == adminCookie {
			return resp.StatusCode, c
		}
	}
	return resp.StatusCode, nil
}

func (d *device) sessions(t *testing.T) (int, []adminSessionView) {
	t.Helper()
	code, b := d.do(t, "GET", "/api/admin/sessions", nil)
	if code != http.StatusOK {
		return code, nil
	}
	return code, decodeJSON[[]adminSessionView](t, b)
}

// 004-AC1: the admin token, given once, opens a session cookie (HttpOnly, SameSite=Strict, scoped to
// the admin API, 12 h). Only the cookie's hash is stored. The session works until it expires.
func TestAdminLoginAndSessionExpiry(t *testing.T) {
	e := newEnv(t)
	clk := e.clock()
	alice := e.device(t)
	if code, c := alice.login(t, "not-the-token", "203.0.113.1"); code != http.StatusUnauthorized || c != nil {
		t.Fatalf("wrong token: %d %v", code, c)
	}
	if code, _ := alice.sessions(t); code != http.StatusUnauthorized {
		t.Fatalf("sessions before login: %d", code)
	}
	code, c := alice.login(t, e.admin, "203.0.113.1")
	if code != http.StatusCreated || c == nil {
		t.Fatalf("login: %d %v", code, c)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/api/admin" || c.MaxAge != 12*3600 || c.Secure {
		t.Fatalf("cookie: %+v", c)
	}
	if _, err := e.st.TouchAdminSession(t.Context(), c.Value, clk.now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the raw cookie is stored: %v", err)
	}
	if _, err := e.st.TouchAdminSession(t.Context(), HashToken(c.Value), clk.now()); err != nil {
		t.Fatalf("the cookie's hash is not stored: %v", err)
	}
	clk.add(time.Hour)
	code, list := alice.sessions(t)
	if code != http.StatusOK || len(list) != 1 || !list[0].Current || list[0].IP != "203.0.113.1" || list[0].UserAgent != "test-browser" ||
		!list[0].LastUsedAt.Equal(clk.now()) || !list[0].ExpiresAt.Equal(list[0].CreatedAt.Add(12*time.Hour)) {
		t.Fatalf("sessions: %d %+v", code, list)
	}
	clk.add(11*time.Hour - time.Second)
	if code, _ := alice.sessions(t); code != http.StatusOK {
		t.Fatalf("just before expiry: %d", code)
	}
	clk.add(time.Second)
	if code, _ := alice.sessions(t); code != http.StatusUnauthorized {
		t.Fatalf("after 12 h: %d", code)
	}

	// Over https, the cookie is Secure.
	req, _ := http.NewRequestWithContext(t.Context(), "POST", e.ts.URL+"/api/admin/login", strings.NewReader(`{"token":"`+e.admin+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if cs := resp.Cookies(); len(cs) != 1 || !cs[0].Secure {
		t.Fatalf("https cookie: %+v", cs)
	}
}

// 004-AC1: the page lists every open session and revokes any; a revoked one gets a 401 on its next
// request, and revoking one's own clears its cookie. The bearer token keeps working for the API.
func TestAdminSessionRevoke(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.device(t), e.device(t)
	alice.login(t, e.admin, "203.0.113.1")
	bob.login(t, e.admin, "203.0.113.2")
	_, list := alice.sessions(t)
	if len(list) != 2 || !list[1].Current || list[0].Current {
		t.Fatalf("sessions: %+v", list)
	}
	bobID := list[0].ID
	if code, _ := alice.do(t, "DELETE", "/api/admin/sessions/"+strconv.FormatInt(bobID, 10), nil); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	if code, _ := bob.sessions(t); code != http.StatusUnauthorized {
		t.Fatalf("revoked session: %d", code)
	}
	if code, _ := alice.sessions(t); code != http.StatusOK {
		t.Fatalf("other session: %d", code)
	}
	for _, id := range []string{strconv.FormatInt(bobID, 10), "x", "99999999999999999999"} {
		if code, _ := alice.do(t, "DELETE", "/api/admin/sessions/"+id, nil); code != http.StatusNotFound {
			t.Fatalf("revoke %s: %d", id, code)
		}
	}

	code, b := e.do(t, "GET", "/api/admin/sessions", e.admin, nil)
	if list := decodeJSON[[]adminSessionView](t, b); code != http.StatusOK || len(list) != 1 || list[0].Current {
		t.Fatalf("sessions by token: %d %s", code, b)
	}
	if code, _ := e.do(t, "GET", "/api/admin/sessions", "wrong", nil); code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: %d", code)
	}
	// A session cookie writes only from this site.
	if code, _ := alice.do(t, "POST", "/api/admin/logout", nil, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Fatalf("cross-site logout: %d", code)
	}

	aliceID := list[1].ID
	req, _ := http.NewRequestWithContext(t.Context(), "DELETE", e.ts.URL+"/api/admin/sessions/"+strconv.FormatInt(aliceID, 10), nil)
	resp, err := alice.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if cs := resp.Cookies(); resp.StatusCode != http.StatusNoContent || len(cs) != 1 || cs[0].MaxAge >= 0 {
		t.Fatalf("own revoke: %d %+v", resp.StatusCode, cs)
	}
	if code, _ := alice.sessions(t); code != http.StatusUnauthorized {
		t.Fatalf("after own revoke: %d", code)
	}

	carol := e.device(t)
	carol.login(t, e.admin, "203.0.113.3")
	if code, _ := carol.do(t, "POST", "/api/admin/logout", nil); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	if code, _ := carol.sessions(t); code != http.StatusUnauthorized {
		t.Fatalf("after logout: %d", code)
	}
}

// 004-AC1: a client IP gets 10 logins per 15 minutes; past them even the right token is refused,
// other IPs are not, the window frees it again, and a success clears the count.
func TestAdminLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	clk := e.clock()
	d := e.device(t)
	for i := range loginAttempts {
		if code, _ := d.login(t, "guess-"+strconv.Itoa(i), "203.0.113.9"); code != http.StatusUnauthorized {
			t.Fatalf("guess %d: %d", i, code)
		}
	}
	if code, _ := d.login(t, "guess", "203.0.113.9"); code != http.StatusTooManyRequests {
		t.Fatalf("past the limit: %d", code)
	}
	if code, _ := d.login(t, e.admin, "203.0.113.9"); code != http.StatusTooManyRequests {
		t.Fatalf("right token past the limit: %d", code)
	}
	if code, _ := d.login(t, e.admin, "203.0.113.10"); code != http.StatusCreated {
		t.Fatalf("other IP: %d", code)
	}
	clk.add(loginWindow)
	if code, _ := d.login(t, e.admin, "203.0.113.9"); code != http.StatusCreated {
		t.Fatalf("after the window: %d", code)
	}
	for i := range loginAttempts {
		if code, _ := d.login(t, "guess-"+strconv.Itoa(i), "203.0.113.9"); code != http.StatusUnauthorized {
			t.Fatalf("guess %d after a success: %d", i, code)
		}
	}
	// Concurrent guesses cannot slip past the count.
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for range 20 {
		wg.Go(func() {
			req, _ := http.NewRequestWithContext(t.Context(), "POST", e.ts.URL+"/api/admin/login", strings.NewReader(`{"token":"guess"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Forwarded-For", "203.0.113.11")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_ = resp.Body.Close()
			mu.Lock()
			codes[resp.StatusCode]++
			mu.Unlock()
		})
	}
	wg.Wait()
	if codes[http.StatusUnauthorized] != loginAttempts || codes[http.StatusTooManyRequests] != 20-loginAttempts {
		t.Fatalf("concurrent guesses: %v", codes)
	}
}

func (e *env) auditLog(t *testing.T, query string) auditResp {
	t.Helper()
	code, b := e.do(t, "GET", "/api/admin/audit"+query, e.admin, nil)
	if code != http.StatusOK {
		t.Fatalf("audit log: %d %s", code, b)
	}
	return decodeJSON[auditResp](t, b)
}

// 004-AC6: every admin mutation writes one entry naming its target, with the name it had; a
// member's own room actions are not admin actions.
func TestEveryAdminMutationIsAudited(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "Pricing")
	if code, b := e.do(t, "PATCH", "/api/rooms/"+room.ID, e.admin, map[string]string{"name": "Pricing v2"}); code != http.StatusOK {
		t.Fatalf("rename: %d %s", code, b)
	}
	inv := e.createInvite(t, room.ID)
	if code, _ := e.do(t, "DELETE", fmt.Sprintf("/api/rooms/%s/invites/%d", room.ID, inv.ID), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}

	alice := e.device(t)
	joined := alice.join(t, room.ID, "Alice")
	tok := joined.Token
	// Member actions: not audited.
	if code, _ := e.do(t, "PATCH", "/api/rooms/"+room.ID, tok, map[string]string{"name": "By Alice"}); code != http.StatusOK {
		t.Fatalf("member rename: %d", code)
	}
	if code, _ := e.do(t, "POST", "/api/rooms/"+room.ID+"/invites", tok, nil); code != http.StatusCreated {
		t.Fatalf("member invite: %d", code)
	}
	tpl := map[string]any{"name": "Critic", "badge": "🧐", "colour": "#112233", "text": "Criticise."}
	code, b := e.do(t, "POST", "/api/rooms/"+room.ID+"/templates", tok, tpl)
	if code != http.StatusCreated {
		t.Fatalf("publish: %d %s", code, b)
	}
	tplID := strconv.FormatInt(decodeJSON[store.Template](t, b).ID, 10)
	sk := alice.uploadSkill(t, "shared", skillZip(t, "stamp", "x"), http.StatusCreated)
	skID := strconv.FormatInt(sk.ID, 10)

	if code, _ := e.do(t, "DELETE", "/api/templates/0"+tplID, e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("remove template: %d", code)
	}
	if code, _ := e.do(t, "DELETE", "/api/skills/"+skID, e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("remove skill: %d", code)
	}
	prof := alice.profile(t).Profile
	profID := strconv.FormatInt(prof.ID, 10)
	// Ids are recorded as numbers, whatever the path spelled.
	if code, _ := e.do(t, "DELETE", "/api/profiles/00"+profID, e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("delete profile: %d", code)
	}
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+room.ID, e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("delete room: %d", code)
	}
	// Refused mutations write nothing.
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+room.ID, e.admin, nil); code != http.StatusNotFound {
		t.Fatalf("delete again: %d", code)
	}

	got := e.auditLog(t, "").Entries
	type row struct{ action, kind, id, name string }
	want := []row{
		{ActionRoomDelete, store.TargetRoom, room.ID, "By Alice"},
		{ActionProfileDelete, store.TargetProfile, profID, "Alice"},
		{ActionSkillRemove, store.TargetSkill, skID, "stamp"},
		{ActionTemplateRemove, store.TargetTemplate, tplID, "Critic"},
		{ActionInviteCreate, store.TargetRoom, room.ID, "Pricing v2"}, // Alice's invite
		{ActionInviteRevoke, store.TargetRoom, room.ID, "Pricing v2"},
		{ActionInviteCreate, store.TargetRoom, room.ID, "Pricing v2"},
		{ActionRoomRename, store.TargetRoom, room.ID, "Pricing v2"},
		{ActionRoomCreate, store.TargetRoom, room.ID, "Pricing"},
	}
	if len(got) != len(want) {
		t.Fatalf("entries: %+v", got)
	}
	for i, w := range want {
		g := got[i]
		if (row{g.Action, g.TargetKind, g.TargetID, g.TargetName}) != w || g.Actor != store.ActorSuperAdmin || !strings.Contains(string(g.Details), `"via":"token"`) {
			t.Errorf("entry %d: %+v %s, want %+v", i, g, g.Details, w)
		}
	}
	if !strings.Contains(string(got[7].Details), `"from":"Pricing"`) || !strings.Contains(string(got[6].Details), fmt.Sprintf(`"invite":%d`, inv.ID)) {
		t.Errorf("details: %s %s", got[7].Details, got[6].Details)
	}
}

// 004-AC6: logins, logouts and revocations are audited too, under the session that acted; the
// log filters by action and target and pages newest first.
func TestAuditLogViewAPI(t *testing.T) {
	e := newEnv(t)
	d := e.device(t)
	d.login(t, e.admin, "203.0.113.1")
	_, list := d.sessions(t)
	sid := list[0].ID
	if code, _ := d.do(t, "DELETE", "/api/admin/sessions/999", nil); code != http.StatusNotFound {
		t.Fatalf("revoke unknown: %d", code)
	}
	for i := range 3 {
		e.createRoom(t, fmt.Sprintf("Room %d", i))
	}
	if code, _ := d.do(t, "POST", "/api/admin/logout", nil); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	all := e.auditLog(t, "").Entries
	if len(all) != 5 || all[0].Action != ActionAdminLogout || all[4].Action != ActionAdminLogin ||
		!strings.Contains(string(all[4].Details), fmt.Sprintf(`"session":%d`, sid)) || !strings.Contains(string(all[4].Details), `"ip":"203.0.113.1"`) {
		t.Fatalf("entries: %+v", all)
	}
	if got := e.auditLog(t, "?action=room.create").Entries; len(got) != 3 {
		t.Fatalf("by action: %+v", got)
	}
	if got := e.auditLog(t, "?target="+url.QueryEscape("room 1")).Entries; len(got) != 1 || got[0].TargetName != "Room 1" {
		t.Fatalf("by name: %+v", got)
	}
	if got := e.auditLog(t, "?target="+all[2].TargetID).Entries; len(got) != 1 || got[0].ID != all[2].ID {
		t.Fatalf("by id: %+v", got)
	}
	if got := e.auditLog(t, "?target=%25").Entries; len(got) != 0 {
		t.Fatalf("a %% matches literally: %+v", got)
	}
	p1 := e.auditLog(t, "?limit=2")
	p2 := e.auditLog(t, "?limit=2&before="+strconv.FormatInt(p1.Next, 10))
	p3 := e.auditLog(t, "?limit=2&before="+strconv.FormatInt(p2.Next, 10))
	if len(p1.Entries) != 2 || len(p2.Entries) != 2 || len(p3.Entries) != 1 || p3.Next != 0 || p2.Entries[0].ID != all[2].ID || len(p1.Actions) != len(AuditActions) {
		t.Fatalf("pages: %+v %+v %+v", p1, p2, p3)
	}
	for _, q := range []string{"?limit=0", "?limit=201", "?limit=x", "?before=-1", "?before=x"} {
		if code, _ := e.do(t, "GET", "/api/admin/audit"+q, e.admin, nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, code)
		}
	}
}

// 004-AC6: the purge runs at start, then on its period, against the server's clock and the
// configured retention.
func TestAuditPurgeLoop(t *testing.T) {
	opts := testOptions
	opts.AuditRetention = 30 * 24 * time.Hour
	e := newEnvOpts(t, opts)
	clk := e.clock()
	ctx := t.Context()
	for _, age := range []time.Duration{31 * 24 * time.Hour, 29 * 24 * time.Hour} {
		if err := e.st.Audit(ctx, store.AuditEntry{At: clk.now().Add(-age), Actor: store.ActorSuperAdmin, Action: ActionRoomCreate,
			TargetKind: store.TargetRoom, TargetID: age.String()}); err != nil {
			t.Fatal(err)
		}
	}
	defer func(d time.Duration) { auditPurgeEvery = d }(auditPurgeEvery)
	auditPurgeEvery = 10 * time.Millisecond
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { e.srv.PurgeAudit(loopCtx); close(done) }()
	waitEntries := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			got, err := e.st.AuditLog(ctx, store.AuditQuery{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("entries: %+v, want %d", got, n)
			}
		}
	}
	waitEntries(1)
	created := clk.now()
	if _, err := e.st.CreateAdminSession(ctx, "h", "ua", "203.0.113.1", created); err != nil {
		t.Fatal(err)
	}
	clk.add(2 * 24 * time.Hour)
	waitEntries(0)
	// The expired session went with the same run, before any new login.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		// Listed as of its creation, the session shows while its row exists.
		if left, err := e.st.AdminSessions(ctx, created); err != nil || len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the loop left the expired admin session")
		}
	}
	cancel()
	<-done
}

// 004-AC19: /admin is the Preact page, its import map admitted by hash and nothing else inline;
// the vendored modules are served.
func TestAdminPage(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Get(e.ts.URL + "/admin") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	m := regexp.MustCompile(`(?s)<script type="importmap">(.*?)</script>`).FindSubmatch(page)
	if resp.StatusCode != http.StatusOK || m == nil {
		t.Fatalf("page: %d", resp.StatusCode)
	}
	sum := sha256.Sum256(m[1])
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self' 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
		t.Fatalf("csp: %s", csp)
	}
	for _, p := range []string{"/static/esm/preact.js", "/static/esm/signals.js", "/static/esm/htm-preact.js", "/static/admin/admin.js"} {
		if code, _ := e.do(t, "GET", p, "", nil); code != http.StatusOK {
			t.Errorf("%s: %d", p, code)
		}
	}
	// The room pages keep their own policy, with no inline script.
	resp, err = http.Get(e.ts.URL + "/join") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if csp := resp.Header.Get("Content-Security-Policy"); strings.Contains(csp, "sha256-") {
		t.Fatalf("join csp: %s", csp)
	}
}

func TestClip(t *testing.T) {
	for in, want := range map[string]string{"": "", "abc": "abc", "abcdef": "abcd", "abé": "abé", "abcé": "abc", "a\xffb": "a�"} {
		if got := clip(in, 4); got != want {
			t.Errorf("clip(%q) = %q, want %q", in, got, want)
		}
	}
}

// A failed database answers 500 on every admin route, and the purge loop survives it.
func TestAdminRoutesFailWithTheStore(t *testing.T) {
	e := newEnv(t)
	d := e.device(t)
	d.login(t, e.admin, "203.0.113.1")
	room := e.createRoom(t, "Doomed")
	core, logs := observer.New(zap.InfoLevel)
	e.srv.log = zap.New(core)
	_ = e.st.Close()
	if code, _ := d.sessions(t); code != http.StatusInternalServerError {
		t.Errorf("session lookup: %d", code)
	}
	if code, _ := e.device(t).login(t, e.admin, "203.0.113.2"); code != http.StatusInternalServerError {
		t.Errorf("login: %d", code)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/admin/sessions"}, {"DELETE", "/api/admin/sessions/1"}, {"GET", "/api/admin/audit"},
	} {
		if code, _ := e.do(t, c.method, c.path, e.admin, nil); code != http.StatusInternalServerError {
			t.Errorf("%s %s: %d", c.method, c.path, code)
		}
	}
	// The action fails too; its name lookup and audit write are logged, not fatal.
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+room.ID, e.admin, nil); code != http.StatusInternalServerError {
		t.Errorf("delete room: %d", code)
	}
	r := httptest.NewRequestWithContext(t.Context(), "POST", "/", nil)
	e.srv.audit(r, ActionRoomCreate, store.TargetRoom, "x", "", nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer func(d time.Duration) { auditPurgeEvery = d }(auditPurgeEvery)
	auditPurgeEvery = time.Millisecond
	time.AfterFunc(20*time.Millisecond, cancel)
	e.srv.PurgeAudit(ctx)
	for _, ev := range []string{"admin_session_lookup_failed", "audit_target_name_failed", "audit_write_failed", "audit_purge_failed", "admin_session_purge_failed"} {
		if logs.FilterMessage(ev).Len() == 0 {
			t.Errorf("no %s logged", ev)
		}
	}
}
