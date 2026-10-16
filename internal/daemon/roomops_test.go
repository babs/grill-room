package daemon

import (
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// backoffDaemon pairs a daemon whose reconnect backoff is an hour: only Connect brings a dropped
// room back within a test.
func backoffDaemon(t *testing.T, rs *roomServer, participant string, ctx context.Context) (*Daemon, Room) {
	t.Helper()
	d := New(Options{Version: "v-test", Image: "test-image", Tokens: NewTokens(filepath.Join(t.TempDir(), "state")), Docker: dockerUp,
		HTTP: &http.Client{}, Log: zap.NewNop(), StatusEvery: 50 * time.Millisecond, PollEvery: 20 * time.Millisecond,
		MinBackoff: time.Hour, MaxBackoff: time.Hour})
	d.Refresh(ctx)
	room, err := ParseRoomURL(rs.ts.URL + "/r/" + rs.room)
	if err != nil {
		t.Fatal(err)
	}
	grant(d, room.String())
	d.SetRoom(ctx, room)
	waitFor(t, "pending pairing", func() bool { return d.Status().Pairing == "pending" })
	s := d.Status()
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/pairings/"+s.ApproveURL[strings.Index(s.ApproveURL, "=")+1:], participant,
		map[string]bool{"approve": true}, http.StatusOK, nil)
	waitFor(t, "connected", func() bool { s := d.Status(); return s.Pairing == "paired" && s.Connection == "connected" })
	return d, room
}

// 003b-AC7: Connect on a room waiting out its backoff reconnects it at once.
func TestConnectCutsBackoff(t *testing.T) {
	rs := newRoomServer(t)
	alice, _ := rs.join(t, "Alice")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, room := backoffDaemon(t, rs, alice, ctx)
	rs.ts.CloseClientConnections()
	waitFor(t, "waiting out the backoff", func() bool {
		s := d.Status()
		return s.Connection == "offline" && strings.Contains(s.Error, "retry in")
	})
	d.SetRoom(ctx, room)
	waitFor(t, "reconnected by Connect", func() bool { return d.Status().Connection == "connected" })
}

// 003b-AC6: removing a room revokes the token on the server, which drops the daemon, and deletes
// the room here. A server that does not answer keeps the room unless forced.
func TestRemoveRoom(t *testing.T) {
	rs := newRoomServer(t)
	alice, _ := rs.join(t, "Alice")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, room := backoffDaemon(t, rs, alice, ctx)
	tok, err := d.o.Tokens.Get(room)
	if err != nil || tok == "" {
		t.Fatalf("token %q %v", tok, err)
	}
	if err := d.o.PluginOptions.Set(room.String(), 1, OptionValues{Name: "p", Values: map[string]any{"key": "secret"}, Sensitive: []string{"key"}}); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveRoom(ctx, room.String(), false); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.o.Tokens.Get(room); got != "" {
		t.Fatal("token kept after remove")
	}
	if len(d.Status().Rooms) != 0 || len(d.o.PluginOptions.Room(room.String())) != 0 {
		t.Fatalf("room left behind: %+v", d.Status().Rooms)
	}
	waitFor(t, "the room drops the daemon", func() bool { return len(rs.daemons(t, alice)) == 0 })
	rs.call(t, "GET", "/api/rooms/"+rs.room+"/daemon/events", tok, nil, http.StatusUnauthorized, nil)

	gone, err := ParseRoomURL("http://127.0.0.1:1/r/" + strconv.Itoa(int(time.Now().UnixNano()%1e6)))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.o.Tokens.Set(gone, "tok"); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveRoom(ctx, gone.String(), false); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("remove from a dead server: %v", err)
	}
	if got, _ := d.o.Tokens.Get(gone); got != "tok" {
		t.Fatal("token dropped although the server kept it")
	}
	if err := d.RemoveRoom(ctx, gone.String(), true); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.o.Tokens.Get(gone); got != "" {
		t.Fatal("forced remove kept the token")
	}
}

// 003b-AC5 003b-AC6: the supervisor pauses a room through its access, keeps the pause, resumes it,
// then removes it through the worker; a worker before protocol 5 cannot remove.
func TestSupervisorPauseAndRemove(t *testing.T) {
	claude := t.TempDir()
	writeClaudeCreds(t, claude, "cfg-secret", time.Now().Add(time.Hour))
	withAccounts := func(o *SupervisorOptions) {
		up, _ := url.Parse("http://127.0.0.1:1")
		a, err := LoadAccounts(AccountsOptions{Paths: o.Paths, ClaudeConfigDir: claude, Upstream: up, Docker: desktopDocker,
			Keyring: &memKeyring{m: map[string]string{}}, Log: zap.NewNop()})
		if err != nil {
			t.Fatal(err)
		}
		o.Accounts = a
	}
	s := testSupervisor(t, strconv.Itoa(Proto), nil, withAccounts)
	room := Room{Base: "https://rooms.example", ID: "abc"}
	s.SetRoom(context.Background(), room)
	key := room.String()
	waitFor(t, "the room's proxy", func() bool { a := lastAccess(s); return a != nil && a.Proxies[key].Bearer != "" })
	id := instance(t, s).ID
	if err := s.SetPaused(id, "https://rooms.example/r/other", true); err == nil {
		t.Fatal("paused a room the server does not have")
	}
	if err := s.SetPaused(id, key, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the pause", func() bool { a := lastAccess(s); return a != nil && a.Proxies[key] == Proxy{Error: pausedReason} })
	saved, err := loadInstance(s.o.Paths.InstanceConfig(id))
	if err != nil || !slices.Equal(saved.Paused, []string{key}) || !slices.Equal(instance(t, s).Paused, []string{key}) {
		t.Fatalf("saved %+v %v", saved, err)
	}
	if err := s.SetPaused(id, key, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "resumed", func() bool { a := lastAccess(s); return a != nil && a.Proxies[key].Bearer != "" })

	if err := s.RemoveRoom(context.Background(), "https://rooms.example/r/nope", false); err == nil {
		t.Fatal("removed a room the server does not have")
	}
	if err := s.SetPaused(id, key, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRoom(context.Background(), key, false); err != nil {
		t.Fatal(err)
	}
	in := instance(t, s)
	saved, err = loadInstance(s.o.Paths.InstanceConfig(id))
	if len(in.Rooms) != 0 || len(in.Paused) != 0 || err != nil || len(saved.Rooms) != 0 || len(saved.Paused) != 0 {
		t.Fatalf("after remove: %+v, saved %+v %v", in, saved, err)
	}

	old := testSupervisor(t, strconv.Itoa(Proto-1), nil, withAccounts)
	old.SetRoom(context.Background(), room)
	waitFor(t, "the old worker runs", func() bool { return instance(t, old).State == "running" })
	if err := old.RemoveRoom(context.Background(), key, false); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("remove through a protocol 4 worker: %v", err)
	}
}
