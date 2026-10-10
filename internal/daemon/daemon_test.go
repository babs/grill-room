package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/bus"
	"github.com/babs/grill-room/internal/creds"
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
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"host-secret","expiresAt":%d}}`, time.Now().Add(time.Hour).UnixMilli())
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return New(Options{Name: "laptop", Version: "v-test", Image: "test-image", Tokens: NewTokens(filepath.Join(dir, "state")), Docker: run,
		Creds: creds.NewSource(creds.Reader{ConfigDir: dir}), HTTP: &http.Client{}, Log: zap.NewNop(),
		StatusEvery: 50 * time.Millisecond, PollEvery: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond})
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

// 001-AC24: a Keychain that never answers shows a clear message in the local page within 5 s.
func TestLockedKeychainInStatus(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	d := newTestDaemon(t, dockerUp)
	d.o.Creds = creds.NewSource(creds.Reader{ConfigDir: t.TempDir(), KeychainTimeout: creds.KeychainTimeout,
		Keychain: func(context.Context, string, string) ([]byte, error) { <-block; return nil, nil }})
	start := time.Now()
	d.Refresh(context.Background())
	if took := time.Since(start); took >= 5*time.Second {
		t.Fatalf("refresh took %s", took)
	}
	s := d.Status()
	if s.Credentials.OK || !strings.Contains(s.Credentials.Error, "Keychain may be locked") || s.CanHost || !strings.Contains(s.Reason, "credentials unreadable") {
		t.Fatalf("status = %+v", s)
	}
}

// 001-AC28 001-AC29: hosting is offered only with Docker running and fresh credentials, and the
// status never carries the token.
func TestReadiness(t *testing.T) {
	d := newTestDaemon(t, nil)
	d.Refresh(context.Background())
	s := d.Status()
	if s.CanHost || s.Reason != "Docker is not running: Cannot connect to the Docker daemon" || !s.Credentials.OK {
		t.Fatalf("docker down: %+v", s)
	}
	d.o.Docker = dockerUp
	d.Refresh(context.Background())
	if s := d.Status(); !s.CanHost || s.Reason != "" || !s.Docker.OK {
		t.Fatalf("ready: %+v", s)
	}
	b, _ := json.Marshal(d.Status())
	if bytes.Contains(b, []byte("host-secret")) {
		t.Fatal("status holds the token")
	}
	dir := t.TempDir()
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"x","expiresAt":%d}}`, time.Now().Add(-time.Minute).UnixMilli())
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	d.o.Creds = creds.NewSource(creds.Reader{ConfigDir: dir})
	d.Refresh(context.Background())
	if s := d.Status(); s.CanHost || s.Reason != "Claude credentials expired on laptop" || !s.Credentials.Expired {
		t.Fatalf("expired: %+v", s)
	}
}

type roomServer struct {
	ts    *httptest.Server
	st    *store.Store
	room  string
	admin string
}

func newRoomServer(t *testing.T) *roomServer {
	t.Helper()
	return newRoomServerOpts(t, web.Options{MaxUploadBytes: 1 << 20, MaxRoomBytes: 1 << 20, MaxAttachmentBytes: 1 << 20})
}

func newRoomServerOpts(t *testing.T, opts web.Options) *roomServer {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "room.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	admin := web.NewToken()
	ts := httptest.NewUnstartedServer(nil)
	ts.Start()
	ts.Config.Handler = web.New(bus.New(st), web.HashToken(admin), ts.URL, opts, zap.NewNop()).Handler()
	t.Cleanup(ts.Close)
	rs := &roomServer{ts: ts, st: st, admin: admin}
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
	Name    string `json:"name"`
	CanHost bool   `json:"can_host"`
	Reason  string `json:"reason"`
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
		return len(ds) == 1 && ds[0].Name == "laptop" && !ds[0].CanHost && strings.HasPrefix(ds[0].Reason, "Docker is not running")
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
	d.o.Creds.Invalidate()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// A replaced room's loop may still be unwinding: its last writes must not reach the status.
func TestReplacedRoomCannotWriteStatus(t *testing.T) {
	d := newTestDaemon(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.SetRoom(ctx, Room{Base: "http://127.0.0.1:1", ID: "old"})
	old := d.roomUpdater(d.gen)
	d.SetRoom(ctx, Room{Base: "http://127.0.0.1:1", ID: "new"})
	old(func(s *Status) { s.Connection, s.Error = "connected", "stale" })
	if s := d.Status(); s.Connection == "connected" || s.Error == "stale" || s.Room != "http://127.0.0.1:1/r/new" {
		t.Fatalf("status = %+v", s)
	}
}
