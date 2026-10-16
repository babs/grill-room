package daemon

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
	"github.com/babs/grill-room/internal/web"
)

// wsCall is one workspace request the room answered: the response body bytes it sent and, for a
// tar, the files in it.
type wsCall struct {
	method, kind, query string
	bytes               int
	files               []string
}

// wsTraffic records the workspace requests a room answers; hook may answer one itself instead.
type wsTraffic struct {
	mu    sync.Mutex
	calls []wsCall
	hook  func(w http.ResponseWriter, r *http.Request) bool
	// room is the room's own handler.
	room http.Handler
}

type countingWriter struct {
	http.ResponseWriter
	buf bytes.Buffer
}

func (c *countingWriter) Write(b []byte) (int, error) {
	c.buf.Write(b)
	return c.ResponseWriter.Write(b)
}

func (tr *wsTraffic) wrap(h http.Handler) http.Handler {
	tr.room = h
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind := path.Base(r.URL.Path)
		if !strings.Contains(r.URL.Path, "/daemon/turns/") || (kind != "manifest" && kind != "workspace") {
			h.ServeHTTP(w, r)
			return
		}
		tr.mu.Lock()
		hook := tr.hook
		tr.mu.Unlock()
		cw := &countingWriter{ResponseWriter: w}
		if hook == nil || !hook(cw, r) {
			h.ServeHTTP(cw, r)
		}
		c := wsCall{method: r.Method, kind: kind, query: r.URL.RawQuery, bytes: cw.buf.Len()}
		if kind == "workspace" {
			rd := tar.NewReader(&cw.buf)
			for hd, err := rd.Next(); err == nil; hd, err = rd.Next() {
				if hd.Typeflag == tar.TypeReg {
					c.files = append(c.files, hd.Name)
				}
			}
		}
		tr.mu.Lock()
		tr.calls = append(tr.calls, c)
		tr.mu.Unlock()
	})
}

func (tr *wsTraffic) setHook(h func(w http.ResponseWriter, r *http.Request) bool) {
	tr.mu.Lock()
	tr.hook = h
	tr.mu.Unlock()
}

// take returns the calls since the last take.
func (tr *wsTraffic) take() []wsCall {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	c := tr.calls
	tr.calls = nil
	return c
}

// rewriteManifest answers manifest requests with the room's manifest after edit.
func (tr *wsTraffic) rewriteManifest(edit func(m *turn.Manifest)) func(w http.ResponseWriter, r *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if path.Base(r.URL.Path) != "manifest" {
			return false
		}
		rec := httptest.NewRecorder()
		tr.room.ServeHTTP(rec, r)
		var m turn.Manifest
		if json.Unmarshal(rec.Body.Bytes(), &m) != nil || len(m.Files) == 0 {
			http.Error(w, "test: no manifest", http.StatusInternalServerError)
			return true
		}
		edit(&m)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m)
		return true
	}
}

// partialTar answers requests for listed files with a tar of files instead.
func partialTar(files map[string]string) func(w http.ResponseWriter, r *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			return false
		}
		tw := tar.NewWriter(w)
		for name, body := range files {
			_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))})
			_, _ = tw.Write([]byte(body))
		}
		_ = tw.Close()
		return true
	}
}

// syncRoom is a room with a 256 KiB upload and a paired host daemon with a workspace cache of
// cacheMax bytes, none for 0; the agent records each turn's workspace.
type syncRoom struct {
	rs      *roomServer
	tr      *wsTraffic
	d       *Daemon
	ag      *fakeAgent
	alice   string
	upload  string
	data    []byte
	room    Room
	workdir []map[string]string
	// write is what the next turn writes to docs/.
	write map[string]string
}

func newSyncRoom(t *testing.T, cacheMax int64) *syncRoom {
	t.Helper()
	sr := &syncRoom{tr: &wsTraffic{}, data: make([]byte, 256<<10)}
	sr.rs = newRoomServerWrapped(t, web.Options{MaxUploadBytes: 1 << 20, MaxRoomBytes: 4 << 20, MaxAttachmentBytes: 1 << 20}, sr.tr.wrap)
	_, _ = rand.Read(sr.data)
	up, err := sr.rs.st.AddUpload(t.Context(), &store.Upload{RoomID: sr.rs.room, Author: "Alice", Filename: "big.pdf",
		ContentType: "application/pdf", Format: "pdf", Data: sr.data}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	sr.upload = fmt.Sprintf("uploads/%d-big.pdf", up.ID)
	sr.ag = &fakeAgent{run: func(_ context.Context, _ sandbox.TurnInput, files map[string]string) (*sandbox.TurnOutput, error) {
		sr.workdir = append(sr.workdir, files)
		var ch []turn.Change
		for p, body := range sr.write {
			ch = append(ch, turn.Change{Path: p, Content: []byte(body)})
		}
		sr.write = nil
		return &sandbox.TurnOutput{Result: &sandbox.Result{Result: "Turn " + strconv.Itoa(len(sr.workdir)), SessionID: sid},
			Session: []byte("x\n"), SessionOffset: int64(len(sr.workdir)-1) * 2, Changes: ch}, nil
	}}
	cacheDir := t.TempDir()
	sr.alice, sr.d, _ = pairedDaemon(t, sr.rs, "Alice", sr.ag, func(d *Daemon) {
		if cacheMax > 0 {
			d.cache = &wsCache{dir: cacheDir, max: cacheMax}
		}
	})
	sr.room, _ = ParseRoomURL(sr.rs.ts.URL + "/r/" + sr.rs.room)
	sr.tr.take() // pairing traffic
	return sr
}

// turn runs one turn and returns the workspace requests it made.
func (sr *syncRoom) turn(t *testing.T) []wsCall {
	t.Helper()
	var started store.Turn
	sr.rs.call(t, "POST", "/api/rooms/"+sr.rs.room+"/turns", sr.alice, map[string]bool{}, http.StatusCreated, &started)
	waitFor(t, "turn done", func() bool {
		v := sr.rs.agent(t, sr.alice)
		return v.Turn.ID == started.ID && v.Turn.Status != "running"
	})
	if v := sr.rs.agent(t, sr.alice); v.Turn.Status != "done" {
		t.Fatalf("turn %+v", v.Turn)
	}
	files := sr.workdir[len(sr.workdir)-1]
	if files[sr.upload] != string(sr.data) {
		t.Fatalf("turn %d: the upload differs in the workspace", len(sr.workdir))
	}
	for _, d := range []string{"docs/", "uploads/", "transcript/", "attachments/"} {
		if _, ok := files[d]; !ok {
			t.Fatalf("turn %d: no %s in the workspace", len(sr.workdir), d)
		}
	}
	return sr.tr.take()
}

func total(calls []wsCall) (n int) {
	for _, c := range calls {
		n += c.bytes
	}
	return n
}

// fetched lists the files the turn's workspace tars carried, sorted.
func fetched(calls []wsCall) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c.files...)
	}
	slices.Sort(out)
	return out
}

func fullFetch(calls []wsCall) string {
	for _, c := range calls {
		if c.method == http.MethodGet && c.kind == "workspace" {
			return "full?" + c.query
		}
	}
	return ""
}

// 002-AC37: the first turn fetches the whole workspace; the second, with no document changed,
// fetches only the transcript day the first synthesis extended, no upload byte; the third, after
// the second turn wrote a document, fetches only that document and the transcript. Each turn's
// sandbox still gets the whole workspace.
func TestWorkspaceSyncTransfersOnlyChanges(t *testing.T) {
	sr := newSyncRoom(t, 64<<20)
	day := "transcript/" + time.Now().UTC().Format(time.DateOnly) + "/001.md"

	first := sr.turn(t)
	if fullFetch(first) != "full?fallback=missing" || total(first) < len(sr.data) || !slices.Contains(fetched(first), sr.upload) {
		t.Fatalf("turn 1: %+v", first)
	}
	sr.write = map[string]string{"docs/a.md": "# A\n"}
	second := sr.turn(t)
	if fullFetch(second) != "" || !slices.Equal(fetched(second), []string{day}) || total(second) >= len(sr.data)/8 {
		t.Fatalf("turn 2: %+v", second)
	}
	third := sr.turn(t)
	if fullFetch(third) != "" || !slices.Equal(fetched(third), []string{"docs/a.md", day}) {
		t.Fatalf("turn 3: %+v", third)
	}
	if sr.workdir[2]["docs/a.md"] != "# A\n" {
		t.Fatalf("turn 3 workspace: %q", sr.workdir[2]["docs/a.md"])
	}
	t.Logf("workspace bytes: turn 1 %d, turn 2 %d, turn 3 %d (upload %d)", total(first), total(second), total(third), len(sr.data))
	if n := sr.rs.srv.WorkspaceFallbacks("missing"); n != 1 {
		t.Fatalf("missing fallbacks %d", n)
	}
}

// 003-AC23: with the transcript in chunks, a turn after new messages fetches only the day's last
// chunk, plus the chunk holding a message edited since: full chunks stay in the cache. The bytes
// are measured as for 002-AC37.
func TestTranscriptChunksTransferOnlyChanges(t *testing.T) {
	sr := newSyncRoom(t, 64<<20)
	ctx := t.Context()
	ps, err := sr.rs.st.Participants(ctx, sr.rs.room)
	if err != nil || len(ps) == 0 {
		t.Fatalf("participants %+v %v", ps, err)
	}
	var first int64
	for i := range 3 * turn.TranscriptChunk {
		m := &store.Message{RoomID: sr.rs.room, Kind: store.KindText, ParticipantID: ps[0].ID, Sender: ps[0].Name,
			Body: fmt.Sprintf("line %d, long enough to weigh a little in the transcript", i)}
		if err := sr.rs.st.InsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = m.ID
		}
	}
	day := "transcript/" + time.Now().UTC().Format(time.DateOnly) + "/"
	whole := sr.turn(t)
	if !slices.Contains(fetched(whole), day+"001.md") || !slices.Contains(fetched(whole), day+"004.md") {
		t.Fatalf("turn 1: %v", fetched(whole))
	}
	// Only the first turn's synthesis is new: it lands in the last chunk.
	appended := sr.turn(t)
	if got := fetched(appended); !slices.Equal(got, []string{day + "004.md"}) {
		t.Fatalf("turn 2 fetched %v", got)
	}
	if _, _, err := sr.rs.st.EditMessage(ctx, sr.rs.room, first, ps[0].ID, "line 0, edited"); err != nil {
		t.Fatal(err)
	}
	edited := sr.turn(t)
	if got := fetched(edited); !slices.Equal(got, []string{day + "001.md", day + "004.md"}) {
		t.Fatalf("turn 3 fetched %v", got)
	}
	chunk := len(sr.workdir[2][day+"002.md"])
	if total(appended) > chunk || total(edited) > 3*chunk {
		t.Fatalf("bytes: turn 2 %d, turn 3 %d, a full chunk %d", total(appended), total(edited), chunk)
	}
	if !strings.Contains(sr.workdir[2][day+"001.md"], "(edited): line 0, edited\n") {
		t.Fatalf("turn 3's 001.md:\n%.300s", sr.workdir[2][day+"001.md"])
	}
	t.Logf("workspace bytes: turn 1 %d, turn 2 %d, turn 3 %d (a full chunk %d)", total(whole), total(appended), total(edited), chunk)
}

// cached returns the path of the cached copy of data.
func (sr *syncRoom) cached(data []byte) string {
	sum := sha256.Sum256(data)
	return filepath.Join(sr.d.cache.roomDir(sr.room), hex.EncodeToString(sum[:]))
}

// 002-AC38: a corrupt cached file or a room cache gone missing falls back to the full workspace:
// the turn succeeds with the right content, the fallback is counted by reason, and the next turn
// is partial again.
func TestWorkspaceSyncFallsBack(t *testing.T) {
	sr := newSyncRoom(t, 64<<20)
	sr.turn(t)
	obj := sr.cached(sr.data)
	if err := os.WriteFile(obj, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if calls := sr.turn(t); fullFetch(calls) != "full?fallback=corrupt" {
		t.Fatalf("corrupt: %+v", calls)
	}
	if calls := sr.turn(t); fullFetch(calls) != "" || slices.Contains(fetched(calls), sr.upload) {
		t.Fatalf("after the corrupt fallback: %+v", calls)
	}
	// A truncated copy is corrupt too.
	if err := os.WriteFile(obj, sr.data[:1000], 0o600); err != nil {
		t.Fatal(err)
	}
	if calls := sr.turn(t); fullFetch(calls) != "full?fallback=corrupt" {
		t.Fatalf("truncated: %+v", calls)
	}
	if err := os.RemoveAll(sr.d.cache.roomDir(sr.room)); err != nil {
		t.Fatal(err)
	}
	if calls := sr.turn(t); fullFetch(calls) != "full?fallback=missing" {
		t.Fatalf("missing: %+v", calls)
	}
	if c, m := sr.rs.srv.WorkspaceFallbacks("corrupt"), sr.rs.srv.WorkspaceFallbacks("missing"); c != 2 || m != 2 {
		t.Fatalf("fallbacks corrupt %d missing %d", c, m)
	}
}

// 002-AC37 002-AC38: a hostile manifest or a partial tar that strays from it never reaches the
// cache or the workspace: the daemon falls back to the full tar and the turn succeeds.
func TestWorkspaceSyncRefusesHostileRoom(t *testing.T) {
	sr := newSyncRoom(t, 64<<20)
	sr.turn(t)
	day := "transcript/" + time.Now().UTC().Format(time.DateOnly) + "/001.md"
	for _, tc := range []struct {
		name, reason string
		hook         func(w http.ResponseWriter, r *http.Request) bool
	}{
		// The manifest names a path out of /workspace, or a digest that is no file name.
		{"traversal", "manifest", sr.tr.rewriteManifest(func(m *turn.Manifest) { m.Files[0].Path = "../escaped" })},
		{"absolute", "manifest", sr.tr.rewriteManifest(func(m *turn.Manifest) { m.Files[0].Path = "/etc/escaped" })},
		{"inner dots", "manifest", sr.tr.rewriteManifest(func(m *turn.Manifest) { m.Files[0].Path = "docs/../../escaped" })},
		{"dir", "manifest", sr.tr.rewriteManifest(func(m *turn.Manifest) { m.Dirs = append(m.Dirs, "docs/../..") })},
		{"digest", "manifest", sr.tr.rewriteManifest(func(m *turn.Manifest) { m.Files[0].SHA256 = "../../escaped" })},
		{"duplicate", "manifest", sr.tr.rewriteManifest(func(m *turn.Manifest) { m.Files = append(m.Files, m.Files[0]) })},
		{"mode", "manifest", sr.tr.rewriteManifest(func(m *turn.Manifest) { m.Files[0].Mode = 0o4755 })},
		// The partial tar strays from what was asked: a path out, a file not asked for, other bytes,
		// or nothing.
		{"unrequested escape", "partial", partialTar(map[string]string{"../escaped": "x"})},
		{"unrequested", "partial", partialTar(map[string]string{"docs/other.md": "x"})},
		{"wrong bytes", "partial", partialTar(map[string]string{day: "not the transcript"})},
		{"empty", "partial", partialTar(nil)},
	} {
		sr.tr.setHook(tc.hook)
		if calls := sr.turn(t); fullFetch(calls) != "full?fallback="+tc.reason {
			t.Fatalf("%s: %+v", tc.name, calls)
		}
		for p := range sr.workdir[len(sr.workdir)-1] {
			if strings.Contains(p, "..") || strings.HasPrefix(p, "/") || p == "docs/other.md" {
				t.Fatalf("%s: %q reached the workspace", tc.name, p)
			}
		}
	}
	// Files nobody asked for are refused before they reach the cache.
	if _, err := os.Stat(sr.cached([]byte("x"))); !os.IsNotExist(err) {
		t.Fatalf("an unrequested file was cached: %v", err)
	}
}

// Version skew: a daemon without a cache, like one from before 002-AC37, fetches the full tar with
// no manifest; a daemon with a cache meeting a room server without manifests falls back to it.
func TestWorkspaceSyncVersionSkew(t *testing.T) {
	sr := newSyncRoom(t, 64<<20)
	sr.tr.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if path.Base(r.URL.Path) == "manifest" || r.Method == http.MethodPost {
			http.NotFound(w, r)
			return true
		}
		return false
	})
	for range 2 {
		if calls := sr.turn(t); len(calls) != 2 || fullFetch(calls) != "full?fallback=manifest" {
			t.Fatalf("older room server: %+v", calls)
		}
	}

	// No cache: the full tar each turn, no manifest, no fallback reason.
	old := newSyncRoom(t, 0)
	for range 2 {
		if calls := old.turn(t); len(calls) != 1 || fullFetch(calls) != "full?" {
			t.Fatalf("daemon without a cache: %+v", calls)
		}
	}
}

// 002-AC37: a room's cache is deleted when the room revokes the daemon, or is deleted.
func TestWorkspaceCacheForgottenWithTheRoom(t *testing.T) {
	for _, how := range []string{"revoke", "delete"} {
		sr := newSyncRoom(t, 64<<20)
		sr.turn(t)
		dir := sr.d.cache.roomDir(sr.room)
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%s: no cache after a turn: %v", how, err)
		}
		if how == "revoke" {
			// Revoking the invite Alice joined with revokes her daemon.
			var invites []struct {
				ID int64 `json:"id"`
			}
			sr.rs.call(t, "GET", "/api/rooms/"+sr.rs.room+"/invites", sr.rs.admin, nil, http.StatusOK, &invites)
			for _, inv := range invites {
				sr.rs.call(t, "DELETE", "/api/rooms/"+sr.rs.room+"/invites/"+strconv.FormatInt(inv.ID, 10)+"?cut=1", sr.rs.admin, nil, http.StatusOK, nil)
			}
		} else {
			sr.rs.call(t, "DELETE", "/api/rooms/"+sr.rs.room, sr.rs.admin, nil, http.StatusNoContent, nil)
		}
		waitFor(t, how+": cache removed", func() bool { _, err := os.Stat(dir); return os.IsNotExist(err) })
	}
}

// 002-AC37: the cache keeps under its cap across rooms by dropping the least recently used files
// first; a cap of 0 or no dir disables it.
func TestWorkspaceCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := &wsCache{dir: t.TempDir(), max: 250}
	old := time.Now().Add(-time.Hour)
	var paths []string
	for i, room := range []string{"a", "b", "a"} {
		dir := filepath.Join(c.dir, room)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, strconv.Itoa(i))
		if err := os.WriteFile(p, make([]byte, 100), 0o600); err != nil {
			t.Fatal(err)
		}
		used := old.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, used, used); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	// A check touches the file it finds intact: the oldest becomes the newest.
	sum := sha256.Sum256(make([]byte, 100))
	f := turn.File{Path: "uploads/1-x", SHA256: hex.EncodeToString(sum[:]), Size: 100}
	if err := os.Rename(paths[0], filepath.Join(c.dir, "a", f.SHA256)); err != nil {
		t.Fatal(err)
	}
	paths[0] = filepath.Join(c.dir, "a", f.SHA256)
	if lack, err := missing(filepath.Join(c.dir, "a"), []turn.File{f}); err != nil || len(lack) != 0 {
		t.Fatalf("missing %v %v", lack, err)
	}
	c.evict()
	for i, want := range []bool{true, false, true} {
		if _, err := os.Stat(paths[i]); (err == nil) != want {
			t.Fatalf("file %d kept=%v, want %v", i, err == nil, want)
		}
	}
	c.max = 1000
	c.evict()
	if _, err := os.Stat(paths[2]); err != nil {
		t.Fatal("evicted under the cap")
	}
	for _, o := range []Options{{CacheDir: c.dir}, {CacheMax: 1 << 20}} {
		if d := New(o); d.cache != nil {
			t.Fatalf("cache on with %+v", o)
		}
	}
	if d := New(Options{CacheDir: c.dir, CacheMax: 1 << 20}); d.cache == nil || d.cache.max != 1<<20 {
		t.Fatal("cache off")
	}
}

// The daemon writes only regular files and dirs at paths inside /workspace: links and paths out
// are refused before anything is written.
func TestIngestRefusesUnsafeEntries(t *testing.T) {
	for _, h := range []tar.Header{
		{Name: "docs/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		{Name: "docs/hard", Typeflag: tar.TypeLink, Linkname: "../../etc/passwd"},
		{Name: "../out", Typeflag: tar.TypeReg},
		{Name: "/abs", Typeflag: tar.TypeReg},
		{Name: "docs/../../out", Typeflag: tar.TypeReg},
		{Name: "../", Typeflag: tar.TypeDir},
		{Name: "dev", Typeflag: tar.TypeChar},
	} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		h.Mode = 0o644
		_ = tw.WriteHeader(&h)
		_ = tw.Close()
		dir := filepath.Join(t.TempDir(), "room")
		if _, _, err := ingest(dir, &buf, nil); err == nil {
			t.Fatalf("%q (type %c) accepted", h.Name, h.Typeflag)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("%q: wrote %v", h.Name, entries)
		}
	}
}

// failedTurn runs one turn that fails and returns its error.
func (sr *syncRoom) failedTurn(t *testing.T) string {
	t.Helper()
	var started store.Turn
	sr.rs.call(t, "POST", "/api/rooms/"+sr.rs.room+"/turns", sr.alice, map[string]bool{}, http.StatusCreated, &started)
	waitFor(t, "turn failed", func() bool {
		v := sr.rs.agent(t, sr.alice)
		return v.Turn.ID == started.ID && v.Turn.Status != "running"
	})
	v := sr.rs.agent(t, sr.alice)
	if v.Turn.Status != "failed" {
		t.Fatalf("turn %+v", v.Turn)
	}
	sr.tr.take()
	return v.Turn.Error
}

// A workspace the room cannot send fails the turn, with or without a cache, and frees the cache
// for the next turn.
func TestWorkspaceSyncFetchFailures(t *testing.T) {
	broken := func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case path.Base(r.URL.Path) == "manifest":
			http.NotFound(w, r)
		case r.Method == http.MethodGet:
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
		default:
			return false
		}
		return true
	}
	cut := func(w http.ResponseWriter, r *http.Request) bool {
		if path.Base(r.URL.Path) == "manifest" {
			http.NotFound(w, r)
			return true
		}
		tw := tar.NewWriter(w)
		_ = tw.WriteHeader(&tar.Header{Name: "docs/cut.md", Mode: 0o644, Size: 100})
		_, _ = tw.Write([]byte("only ten b"))
		return true
	}
	for _, cacheMax := range []int64{64 << 20, 0} {
		sr := newSyncRoom(t, cacheMax)
		for _, hook := range []func(http.ResponseWriter, *http.Request) bool{broken, cut} {
			sr.tr.setHook(hook)
			if e := sr.failedTurn(t); !strings.Contains(e, "could not fetch the workspace") && !strings.Contains(e, "unexpected EOF") {
				t.Fatalf("cache %d: %q", cacheMax, e)
			}
		}
		sr.tr.setHook(nil)
		sr.turn(t)
	}
}
