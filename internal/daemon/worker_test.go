package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// fakeSupervisor is the supervisor end of a worker's pipes: it keeps the latest status and
// collects replies and logs.
type fakeSupervisor struct {
	conn    *Conn
	w       io.WriteCloser
	mu      sync.Mutex
	status  *Status
	metrics *Metrics
	replies map[int64]Msg
	logs    []string
	next    int64
}

func newFakeSupervisor(t *testing.T) (*fakeSupervisor, *Conn) {
	t.Helper()
	supR, workerW := io.Pipe()
	workerR, supW := io.Pipe()
	f := &fakeSupervisor{conn: NewConn(supR, supW), w: supW, replies: map[int64]Msg{}}
	go func() {
		for {
			m, err := f.conn.Recv()
			if err != nil {
				return
			}
			f.mu.Lock()
			switch m.Type {
			case "status":
				f.status = m.Status
			case "metrics":
				f.metrics = m.Metrics
			case "reply":
				f.replies[m.ID] = m
			case "log":
				f.logs = append(f.logs, string(m.Log))
			}
			f.mu.Unlock()
		}
	}()
	t.Cleanup(func() { _ = supR.Close(); _ = workerW.Close() })
	return f, NewConn(workerR, workerW)
}

func (f *fakeSupervisor) request(t *testing.T, m Msg) Msg {
	t.Helper()
	f.mu.Lock()
	f.next++
	m.ID = f.next
	f.mu.Unlock()
	if err := f.conn.Send(m); err != nil {
		t.Fatal(err)
	}
	var r Msg
	waitFor(t, "reply to "+m.Type, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		var ok bool
		r, ok = f.replies[m.ID]
		return ok
	})
	return r
}

func (f *fakeSupervisor) lastStatus() Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status == nil {
		return Status{}
	}
	return *f.status
}

// 003-AC9: the worker side of the protocol. It reconnects to the newest room holding a token,
// pushes its status as it changes and its metrics, answers each request, sends its logs, and
// returns on stop once its rooms ended.
func TestServeWorker(t *testing.T) {
	rs := newRoomServer(t)
	paired, _ := ParseRoomURL(rs.ts.URL + "/r/" + rs.room)
	d := newTestDaemon(t, nil)
	f, conn := newFakeSupervisor(t)
	d.o.Log = zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), LogWriter{Conn: conn}, zapcore.InfoLevel))
	if err := d.o.Tokens.Set(paired, rs.tokenFor(t, paired)); err != nil {
		t.Fatal(err)
	}
	unpaired := Room{Base: rs.ts.URL, ID: "zzz"}
	done := make(chan error, 1)
	go func() {
		done <- ServeWorker(context.Background(), conn, d, WorkerConfig{Rooms: []string{paired.String(), unpaired.String(), "bad url"}})
	}()
	waitFor(t, "the paired room connected", func() bool { s := f.lastStatus(); return s.Room == paired.String() && s.Connection == "connected" })
	waitFor(t, "metrics", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.metrics != nil && f.metrics.Goroutines > 0 })

	if r := f.request(t, Msg{Type: "settings", Settings: &Settings{MaxModel: "claude-sonnet-5-5"}}); r.Error != "" {
		t.Fatalf("settings: %+v", r)
	}
	waitFor(t, "the new cap in the status", func() bool { return f.lastStatus().Settings.MaxModel == "sonnet" })
	if r := f.request(t, Msg{Type: "allowlist"}); r.Error != "" {
		t.Fatalf("empty allowlist: %+v", r)
	}
	waitFor(t, "the empty allowlist", func() bool { s := f.lastStatus(); return s.Allowlist != nil && len(s.Allowlist) == 0 })
	for _, bad := range []Msg{{Type: "settings"}, {Type: "settings", Settings: &Settings{MaxEffort: "turbo"}}, {Type: "pair", Room: "http://x/r/a"},
		{Type: "update"}, {Type: "dance"}, {Type: "allowlist", Allowlist: []string{"not a domain!"}}} {
		if r := f.request(t, bad); r.Error == "" {
			t.Errorf("%+v answered without error", bad)
		}
	}
	if r := f.request(t, Msg{Type: "denied", N: 5}); r.Error != "" || r.Denied == nil && len(r.Denied) != 0 {
		t.Fatalf("denied: %+v", r)
	}
	if r := f.request(t, Msg{Type: "recheck"}); r.Error != "" {
		t.Fatalf("recheck: %+v", r)
	}
	if r := f.request(t, Msg{Type: "pair", Room: unpaired.String()}); r.Error != "" {
		t.Fatalf("pair: %+v", r)
	}
	waitFor(t, "the other room", func() bool { return f.lastStatus().Room == unpaired.String() })

	if err := f.conn.Send(Msg{Type: "stop"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not stop")
	}
	waitFor(t, "the logs", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		logs := strings.Join(f.logs, "\n")
		return strings.Contains(logs, `"msg":"settings_changed"`) && strings.Contains(logs, `"msg":"worker_stopping"`)
	})
}

// tokenFor pairs a throwaway daemon with room and returns the token the room issued.
func (rs *roomServer) tokenFor(t *testing.T, room Room) string {
	t.Helper()
	participant, _ := rs.join(t, "Pairer")
	d := newTestDaemon(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	d.SetRoom(ctx, room)
	waitFor(t, "pending", func() bool { return d.Status().Pairing == "pending" })
	id := d.Status().ApproveURL[strings.Index(d.Status().ApproveURL, "=")+1:]
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/pairings/"+id, participant, map[string]bool{"approve": true}, 200, nil)
	waitFor(t, "paired", func() bool { return d.Status().Pairing == "paired" })
	cancel()
	d.Wait()
	tok, _ := d.o.Tokens.Get(room)
	return tok
}

// A stopped supervisor closes the pipe: the worker stops as on a stop message.
func TestServeWorkerStopsOnEOF(t *testing.T) {
	d := newTestDaemon(t, nil)
	f, conn := newFakeSupervisor(t)
	done := make(chan error, 1)
	go func() { done <- ServeWorker(context.Background(), conn, d, WorkerConfig{}) }()
	waitFor(t, "a first status", func() bool { return f.lastStatus().Version == "v-test" })
	_ = f.w.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not stop on EOF")
	}
}

func TestConnAndLogWriter(t *testing.T) {
	var buf bytes.Buffer
	c := NewConn(&buf, &buf)
	if _, err := (LogWriter{Conn: c}).Write([]byte("not json\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := (LogWriter{Conn: c}).Write([]byte(`{"msg":"x"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`{"msg":"not json"}`, `{"msg":"x"}`} {
		m, err := c.Recv()
		if err != nil || m.Type != "log" || string(m.Log) != want {
			t.Fatalf("got %+v %s %v", m, m.Log, err)
		}
	}
	if _, err := c.Recv(); err != io.EOF {
		t.Fatalf("end: %v", err)
	}
	if _, err := NewConn(strings.NewReader("{bad\n"), io.Discard).Recv(); err == nil {
		t.Fatal("a bad line passed")
	}
	long := `{"type":"log","log":"` + strings.Repeat("x", maxLine) + `"}` + "\n"
	if _, err := NewConn(strings.NewReader(long), io.Discard).Recv(); err == nil {
		t.Fatal("an oversized line passed")
	}
	if err := (LogWriter{Conn: c}).Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	b, _ := json.Marshal(Msg{Type: "allowlist", Allowlist: []string{}})
	if string(b) != `{"type":"allowlist"}` {
		t.Fatalf("an empty allowlist travels as %s", b)
	}
}

// A worker pairs with the room it is handed; with none, it serves the newest room holding a token.
func TestStartRoom(t *testing.T) {
	d := newTestDaemon(t, nil)
	a, b := Room{Base: "https://rooms.example", ID: "aaa"}, Room{Base: "https://rooms.example", ID: "bbb"}
	if err := d.o.Tokens.Set(a, "tok"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		cfg  WorkerConfig
		want string
	}{
		{WorkerConfig{Pair: b.String(), Rooms: []string{a.String()}}, b.String()},
		{WorkerConfig{Pair: "http://rooms.example/r/x", Rooms: []string{a.String()}}, ""},
		{WorkerConfig{Rooms: []string{a.String(), b.String()}}, a.String()},
		{WorkerConfig{Rooms: []string{b.String()}}, ""},
		{WorkerConfig{}, ""},
	} {
		r, ok := d.startRoom(tc.cfg)
		if got := map[bool]string{true: r.String(), false: ""}[ok]; got != tc.want {
			t.Errorf("startRoom(%+v) = %q, want %q", tc.cfg, got, tc.want)
		}
	}
}

// Garbage on the supervisor's pipe stops the worker cleanly, as a closed pipe does.
func TestServeWorkerStopsOnGarbage(t *testing.T) {
	d := newTestDaemon(t, nil)
	f, conn := newFakeSupervisor(t)
	done := make(chan error, 1)
	go func() { done <- ServeWorker(context.Background(), conn, d, WorkerConfig{}) }()
	waitFor(t, "a first status", func() bool { return f.lastStatus().Version == "v-test" })
	if _, err := f.w.(io.Writer).Write([]byte("{not json\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not stop on garbage")
	}
}
