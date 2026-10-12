package web

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/archive"
	"github.com/babs/grill-room/internal/store"
)

// send posts body as an archive to path, with a bearer token when tok is set, the device's cookies
// when d is set, and as another site's request when crossSite is.
func (e *env) send(t *testing.T, d *device, path, tok string, body []byte, ctype string, crossSite ...bool) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), "POST", e.ts.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", ctype)
	if len(crossSite) > 0 && crossSite[0] {
		req.Header.Set("Sec-Fetch-Site", "cross-site")
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	c := http.DefaultClient
	if d != nil {
		c = d.c
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// retar rewrites an archive entry by entry through edit, which may change the header, the manifest
// included.
func retar(t *testing.T, b []byte, edit func(h *tar.Header, data []byte) []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	tw := tar.NewWriter(gw)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		data = edit(h, data)
		h.Size = int64(len(data))
		if h.Typeflag != tar.TypeReg {
			h.Size, data = 0, nil
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(data)
	}
	_ = tw.Close()
	_ = gw.Close()
	return out.Bytes()
}

// stored is a gzip'd tar of n entries of size bytes each, not compressed: its body is as large as
// its content.
func stored(t *testing.T, n, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw, _ := gzip.NewWriterLevel(&buf, gzip.NoCompression)
	tw := tar.NewWriter(gw)
	for i := range n {
		if err := tw.WriteHeader(&tar.Header{Name: string(rune('a' + i)), Mode: 0o644, Size: int64(size)}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(bytes.Repeat([]byte{'x'}, size))
	}
	_ = tw.Close()
	_ = gw.Close()
	return buf.Bytes()
}

// 004-AC11 004-AC12 004-AC13 004-AC14 004-AC15 004-AC6: a room admin and the super-admin export a
// room as a tar.gz, audited; a preview reports it and creates nothing; an import makes a new room,
// audited, with its participants relinked; a profile importing becomes its admin; archives that
// must be refused are, with their reason, and create nothing.
func TestExportImportAPI(t *testing.T) {
	e := newEnv(t)
	e.srv.opts.DBPath = e.dbPath
	alice := e.device(t)
	room := alice.createRoom(t, "Pricing", "Alice")
	path := "/api/rooms/" + room.ID
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	if code, b := e.do(t, "POST", path+"/messages", bob.Token, map[string]string{"body": "hello from Bob"}); code != http.StatusCreated {
		t.Fatalf("post: %d %s", code, b)
	}
	if _, err := e.st.AddUpload(t.Context(), &store.Upload{RoomID: room.ID, Author: "Bob", Filename: "plan.md", ContentType: "text/markdown; charset=utf-8",
		Format: "md", Data: []byte("# Plan\n\nShip it.\n"), Text: "# Plan\n\nShip it.\n"}, 1<<20); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.do(t, "GET", path+"/export", bob.Token, nil); code != http.StatusForbidden {
		t.Fatalf("a plain member exports: %d", code)
	}
	if code, _ := e.do(t, "GET", "/api/rooms/nope/export", e.admin, nil); code != http.StatusNotFound {
		t.Fatalf("unknown room: %d", code)
	}
	code, ar := alice.do(t, "GET", path+"/export", nil)
	if code != http.StatusOK {
		t.Fatalf("room admin export: %d %s", code, ar)
	}
	code, adm := e.do(t, "GET", "/api/admin/rooms/"+room.ID+"/export", e.admin, nil)
	if code != http.StatusOK {
		t.Fatalf("super-admin export: %d", code)
	}
	m, err := archive.Unpack(bytes.NewReader(adm), t.TempDir(), archive.Limits{MaxBytes: 1 << 20, MaxFiles: 100})
	if err != nil || m.Room.Name != "Pricing" || m.Room.ID != room.ID || m.Format != archive.Format {
		t.Fatalf("archive: %+v %v", m, err)
	}
	var exports int
	for _, en := range e.auditLog(t, "?action="+ActionRoomExport).Entries {
		if en.TargetID == room.ID {
			exports++
		}
	}
	if exports != 2 {
		t.Fatalf("%d export entries, want 2", exports)
	}
	// Nothing stays staged once the answer is out.
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(e.dbPath), stagingPrefix+"*")); len(left) != 0 {
		t.Fatalf("staging left behind: %v", left)
	}

	rooms := func() int {
		rs, _ := e.st.Rooms(t.Context())
		return len(rs)
	}
	before := rooms()
	code, b := e.send(t, nil, "/api/rooms/import?preview=1", e.admin, ar, "application/gzip")
	pv := decodeJSON[importResp](t, b)
	if code != http.StatusOK || pv.Room != nil || pv.Name != "Pricing" || pv.Counts["messages"] == 0 || pv.Counts["participants"] != 2 ||
		!pv.SameInstance || pv.Format != 1 || pv.ArchiveBytes == 0 || rooms() != before {
		t.Fatalf("preview: %d %s", code, b)
	}
	code, b = e.send(t, nil, "/api/rooms/import", e.admin, ar, "application/gzip")
	got := decodeJSON[importResp](t, b)
	if code != http.StatusCreated || got.Room == nil || got.Room.ID == room.ID || got.Room.Name != "Pricing" || got.Relinked != 2 {
		t.Fatalf("import: %d %s", code, b)
	}
	// Alice's profile opens the copy, where she is admin again; Bob's old token does not.
	if code, b := alice.do(t, "GET", "/api/rooms/"+got.Room.ID, nil); code != http.StatusOK || !decodeJSON[roomResp](t, b).Me.Admin {
		t.Fatalf("alice in the copy: %d %s", code, b)
	}
	if code, _ := e.do(t, "GET", "/api/rooms/"+got.Room.ID, bob.Token, nil); code != http.StatusUnauthorized {
		t.Fatalf("bob's old token opens the copy: %d", code)
	}
	imports := e.auditLog(t, "?action="+ActionRoomImport).Entries
	if len(imports) != 1 || imports[0].TargetID != got.Room.ID || imports[0].Actor != store.ActorSuperAdmin {
		t.Fatalf("import audit: %+v", imports)
	}

	// A profile imports through its own cookie and becomes the copy's admin, seated by name.
	dave := e.device(t)
	dave.createRoom(t, "Dave's", "Dave")
	if code, b := e.send(t, dave, "/api/rooms/import", "", ar, "application/gzip"); code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "display name") {
		t.Fatalf("no display name: %d %s", code, b)
	}
	code, b = e.send(t, dave, "/api/rooms/import?display_name=Dave", "", ar, "application/gzip")
	if code != http.StatusCreated {
		t.Fatalf("profile import: %d %s", code, b)
	}
	// Dave administers nothing the archive names: Alice and Bob stay hints, unlinked.
	if got := decodeJSON[importResp](t, b); got.Relinked != 0 || got.Unlinked != 2 {
		t.Fatalf("dave relinked others: %s", b)
	}
	daves := decodeJSON[importResp](t, b).Room.ID
	if code, b := dave.do(t, "GET", "/api/rooms/"+daves, nil); code != http.StatusOK || !decodeJSON[roomResp](t, b).Me.Admin || decodeJSON[roomResp](t, b).Me.Name != "Dave" {
		t.Fatalf("dave in his import: %d %s", code, b)
	}

	before = rooms()
	edit := func(target string, f func(*tar.Header, []byte) []byte) []byte {
		return retar(t, ar, func(h *tar.Header, data []byte) []byte {
			if h.Name == target {
				return f(h, data)
			}
			return data
		})
	}
	small := newEnvOpts(t, Options{MaxUploadBytes: 100, MaxRoomBytes: 100, MaxAttachmentBytes: 100, MaxImportBytes: 600})
	tight := newEnvOpts(t, Options{MaxUploadBytes: 20000, MaxRoomBytes: 10, MaxAttachmentBytes: 1000})
	for name, c := range map[string]struct {
		e      *env
		d      *device
		body   []byte
		ctype  string
		status int
		want   string
	}{
		"tampered": {e, nil, edit("messages.jsonl", func(_ *tar.Header, b []byte) []byte { return bytes.Replace(b, []byte("Bob"), []byte("Eve"), 1) }),
			"application/gzip", 422, "sha256 mismatch on messages.jsonl"},
		"newer format": {e, nil, edit(archive.ManifestName, func(_ *tar.Header, b []byte) []byte {
			return bytes.Replace(b, []byte(`"format": 1`), []byte(`"format": 2`), 1)
		}), "application/gzip", 422, "format 2 is newer"},
		"traversal": {e, nil, edit("room.json", func(h *tar.Header, b []byte) []byte { h.Name = "../room.json"; return b }), "application/gzip", 422, "unsafe path"},
		"symlink": {e, nil, edit("room.json", func(h *tar.Header, b []byte) []byte {
			h.Typeflag, h.Linkname = tar.TypeSymlink, "/etc/passwd"
			return b
		}),
			"application/gzip", 422, "a symlink"},
		"not gzip":   {e, nil, []byte("hello"), "application/gzip", 422, "not a gzip archive"},
		"bomb":       {small, nil, ar, "application/gzip", 422, "over 600 bytes once uncompressed"},
		"oversize":   {small, nil, stored(t, 3, 300), "application/gzip", 413, "over the 600 bytes"},
		"room quota": {tight, nil, ar, "application/gzip", 422, "room quota"},
		"wrong type": {e, nil, ar, "application/json", 415, "application/gzip"},
		"no profile": {e, e.device(t), ar, "application/gzip", 401, "no profile"},
		"cross-site": {e, alice, ar, "application/gzip", 403, "cross-site"},
	} {
		tok := ""
		if c.d == nil {
			tok = c.e.admin
		}
		code, b := c.e.send(t, c.d, "/api/rooms/import?display_name=A", tok, c.body, c.ctype, name == "cross-site")
		if code != c.status || !strings.Contains(string(b), c.want) {
			t.Errorf("%s: %d %s, want %d %q", name, code, b, c.status, c.want)
		}
	}
	// Alice administers the exported room: her import relinks its participants.
	if code, b := e.send(t, alice, "/api/rooms/import?display_name=A", "", ar, "application/gzip"); code != http.StatusCreated || decodeJSON[importResp](t, b).Relinked != 2 {
		t.Fatalf("alice imports: %d %s", code, b)
	}
	if n := rooms(); n != before+1 {
		t.Fatalf("refusals created %d rooms", n-before-1)
	}
	// Another instance, on another version, says so in the preview.
	other := newEnv(t)
	other.srv.opts.Version = "v9.9.9"
	code, b = other.send(t, nil, "/api/rooms/import?preview=1", other.admin, ar, "application/gzip")
	if pv := decodeJSON[importResp](t, b); code != http.StatusOK || pv.SameInstance || pv.Relinked != 0 || pv.Unlinked != 2 ||
		!strings.Contains(strings.Join(pv.Warnings, " "), "another instance") || !strings.Contains(strings.Join(pv.Warnings, " "), "this instance runs v9.9.9") {
		t.Fatalf("other instance preview: %d %s", code, b)
	}
	// A profile within the rooms limit only, and cross-site writes are refused.
	e.setSettings(t, store.InstanceSettings{RoomsPerProfile: 1, RoomCreation: store.CreationAnyone})
	if code, b := e.send(t, dave, "/api/rooms/import?display_name=Dave", "", ar, "application/gzip"); code != http.StatusForbidden || !strings.Contains(string(b), "rooms") {
		t.Fatalf("past the limit: %d %s", code, b)
	}
}

// 004-AC11: staging left by a server stopped mid-export goes at the next start; a third export at
// once is turned away.
func TestArchiveStagingAndSlots(t *testing.T) {
	e := newEnv(t)
	left := filepath.Join(filepath.Dir(e.dbPath), stagingPrefix+"crashed")
	if err := os.MkdirAll(filepath.Join(left, "uploads"), 0o700); err != nil {
		t.Fatal(err)
	}
	New(e.bus, HashToken(e.admin), baseURL, Options{DBPath: e.dbPath}, zap.NewNop())
	if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging left behind: %v", err)
	}
	room := e.createRoom(t, "Busy")
	for range maxConcurrentArchives {
		e.srv.archives <- struct{}{}
	}
	if code, b := e.do(t, "GET", "/api/rooms/"+room.ID+"/export", e.admin, nil); code != http.StatusServiceUnavailable || !strings.Contains(string(b), "retry") {
		t.Fatalf("busy: %d %s", code, b)
	}
	if code, _ := e.send(t, nil, "/api/rooms/import", e.admin, []byte("x"), "application/gzip"); code != http.StatusServiceUnavailable {
		t.Fatalf("busy import: %d", code)
	}
}

// 004-AC14: an imported persona passes the checks of the room API.
func TestCheckPersona(t *testing.T) {
	good := store.PersonaFields{Name: "Grill", Badge: "🔥", Colour: "#d9480f", Text: "Grill them."}
	if err := checkPersona(good, 0, 0); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(*store.PersonaFields) int{
		"colour":  func(p *store.PersonaFields) int { p.Colour = "red;background:url(x)"; return 0 },
		"name":    func(p *store.PersonaFields) int { p.Name = "two words"; return 0 },
		"model":   func(p *store.PersonaFields) int { p.Model = "gpt"; return 0 },
		"every_n": func(_ *store.PersonaFields) int { return 1000 },
	} {
		p := good
		if err := checkPersona(p, f(&p), 0); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
