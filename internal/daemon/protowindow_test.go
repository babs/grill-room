package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
)

// nextWorker copies this test binary as worker-next: run as a worker, it speaks Proto+2.
func nextWorker(t *testing.T) string {
	t.Helper()
	name := "worker-next"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(t.TempDir(), name)
	src, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()                                            //nolint:errcheck // read-only
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o700) //nolint:gosec // a test binary
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, src); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	// On a Windows runner the first start of a new executable stalled up to 6 s, later ones took
	// milliseconds: pay it here, before a test's 10 s wait starts.
	cmd := exec.CommandContext(t.Context(), dst, "worker")
	cmd.Env = append(os.Environ(), "GRILL_ROOM_TEST_WORKER=early")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return dst
}

// daemon-r2: a server hands out a release whose workers speak Proto+2, beyond this supervisor's
// window, and its version endpoint does not say so. The worker fails the handshake; the supervisor
// runs its own binary instead, so the room stays hosted, and the page asks for an update.
func TestIncompatibleWorkerKeepsHosting(t *testing.T) {
	next := nextWorker(t)
	var asked atomic.Int32
	s := testSupervisor(t, "plain", nil, func(o *SupervisorOptions) {
		o.Binary = func(_ context.Context, in Instance) (Resolved, error) {
			asked.Add(1)
			if in.Avoid == "v-next" {
				return Resolved{Path: os.Args[0], Version: "v-test", Served: "v-next", Trust: "the supervisor's own binary",
					Blocked: "update the app to keep hosting"}, nil
			}
			return Resolved{Path: next, Version: "v-next", Served: "v-next", Trust: "signed"}, nil
		}
	})
	if err := s.SetRoom(context.Background(), Room{Base: "https://rooms.example", ID: "abc"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the room hosted on the supervisor's own binary", func() bool {
		in := instance(t, s)
		return in.State == "running" && in.Version == "v-test" && s.Status().Connection == "connected"
	})
	in := instance(t, s)
	if in.UpdateNeeded != "update the app to keep hosting" || in.Proto != Proto || asked.Load() != 2 {
		t.Fatalf("instance %+v after %d resolves", in, asked.Load())
	}
}

// daemon-r2: the worker reports its server now hands out another version. When Binary keeps the
// running one, as it does for a version outside the protocol window, the worker is not restarted.
func TestServedVersionOutsideWindowKeepsWorker(t *testing.T) {
	var asked atomic.Int32
	s := testSupervisor(t, "served", nil, func(o *SupervisorOptions) {
		o.Binary = func(_ context.Context, _ Instance) (Resolved, error) {
			if asked.Add(1) == 1 {
				return ownBinary(context.Background(), Instance{})
			}
			return Resolved{Path: os.Args[0], Version: "v-test", Served: "v9.9.9", Blocked: "update the app to keep hosting"}, nil
		}
	})
	if err := s.SetRoom(context.Background(), Room{Base: "https://rooms.example", ID: "abc"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the served version checked", func() bool { return instance(t, s).UpdateNeeded != "" })
	if in := instance(t, s); in.State != "running" || in.Restarts != 0 || asked.Load() != 2 {
		t.Fatalf("instance %+v after %d resolves", in, asked.Load())
	}
}

// daemon-r2: Resolve sends the supervisor's protocol range, and a server whose release speaks a
// protocol outside it gets the copy the instance ran last, or the supervisor's own binary.
func TestResolveOutsideProtocolWindow(t *testing.T) {
	lo, hi := ProtoRange()
	for _, tc := range []struct {
		name  string
		proto int
		avoid string
	}{{"newer", hi + 1, ""}, {"older", lo - 1, ""}, {"refused at handshake", 0, "v9.9.9"}} {
		t.Run(tc.name, func(t *testing.T) {
			var query string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/daemon/version" {
					t.Errorf("fetched %s: nothing should be downloaded", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				query = r.URL.Query().Get("proto")
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"version":"v9.9.9","proto":`+strconv.Itoa(tc.proto)+`,"sha256":{"`+target+`":"00"}}`)
			}))
			defer srv.Close()
			b := &Binaries{Dir: t.TempDir(), Self: "/self", Version: "v0.0.5", HTTP: srv.Client(), Log: zap.NewNop()}
			r, err := b.Resolve(context.Background(), Instance{ID: "i1", URL: srv.URL, Avoid: tc.avoid})
			if err != nil {
				t.Fatal(err)
			}
			if query != strconv.Itoa(lo)+"-"+strconv.Itoa(hi) {
				t.Fatalf("proto query %q", query)
			}
			if r.Path != "/self" || r.Served != "v9.9.9" || r.Blocked != urlHost(srv.URL)+" hands out v9.9.9, which this app (v0.0.5) cannot run" {
				t.Fatalf("resolved %+v", r)
			}
		})
	}
}

// daemon-r2: a Binary that hands the avoided version out again ends refused after the second
// handshake, instead of restarting it forever.
func TestIncompatibleWorkerHandedOutAgainIsRefused(t *testing.T) {
	next := nextWorker(t)
	var asked atomic.Int32
	s := testSupervisor(t, "plain", nil, func(o *SupervisorOptions) {
		o.Binary = func(context.Context, Instance) (Resolved, error) {
			asked.Add(1)
			return Resolved{Path: next, Version: "v-next", Served: "v-next"}, nil
		}
	})
	if err := s.SetRoom(context.Background(), Room{Base: "https://rooms.example", ID: "abc"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the worker refused", func() bool { return instance(t, s).State == "refused" })
	waitEnded(t, s)
	if asked.Load() != 2 {
		t.Fatalf("%d resolves", asked.Load())
	}
}
