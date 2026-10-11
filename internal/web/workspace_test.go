package web

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/turn"
)

func untar(t *testing.T, b []byte) map[string]tarFile {
	t.Helper()
	files := map[string]tarFile{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		files[h.Name] = tarFile{mode: h.Mode, data: string(data)}
	}
}

// 002-AC37: the manifest lists every file of the full workspace with its sha256, size and mode,
// sorted by path; a POST returns a tar of only the listed paths, byte for byte as in the full one.
func TestWorkspaceManifestAndPartialTar(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tn := e.press(t, r, r.alice, r.alice.p.Token, false)
	ids := strconv.FormatInt(tn.ID, 10)
	full := e.workspace(t, r, r.alice, tn.ID)

	code, b := e.do(t, "GET", r.daemonPath("turns/"+ids+"/manifest"), r.alice.tok, nil)
	if code != http.StatusOK {
		t.Fatalf("manifest: %d %s", code, b)
	}
	m := decodeJSON[turn.Manifest](t, b)
	if !slices.Equal(m.Dirs, workspaceDirs) {
		t.Fatalf("dirs %v", m.Dirs)
	}
	if !slices.IsSortedFunc(m.Files, func(a, b turn.File) int { return strings.Compare(a.Path, b.Path) }) {
		t.Fatalf("manifest not sorted: %+v", m.Files)
	}
	files := 0
	for name, f := range full {
		if strings.HasSuffix(name, "/") {
			continue
		}
		files++
		i := slices.IndexFunc(m.Files, func(mf turn.File) bool { return mf.Path == name })
		if i < 0 {
			t.Fatalf("%s missing from the manifest", name)
		}
		sum := sha256.Sum256([]byte(f.data))
		if mf := m.Files[i]; mf.SHA256 != hex.EncodeToString(sum[:]) || mf.Size != int64(len(f.data)) || mf.Mode != f.mode {
			t.Fatalf("%s: manifest %+v, tar mode %o size %d", name, mf, f.mode, len(f.data))
		}
	}
	if files != len(m.Files) || files < 6 {
		t.Fatalf("manifest lists %d files, the tar %d", len(m.Files), files)
	}
	// The same turn gives the same manifest.
	if _, b2 := e.do(t, "GET", r.daemonPath("turns/"+ids+"/manifest"), r.alice.tok, nil); !bytes.Equal(b, b2) {
		t.Fatal("manifest not deterministic")
	}

	pdf := fmt.Sprintf("uploads/%d-brief.pdf", r.pdf)
	code, b = e.do(t, "POST", r.daemonPath("turns/"+ids+"/workspace"), r.alice.tok, turn.PathsRequest{Paths: []string{pdf, turn.AnnotationsFile}})
	if code != http.StatusOK {
		t.Fatalf("partial: %d %s", code, b)
	}
	part := untar(t, b)
	if len(part) != 2 || part[pdf] != full[pdf] || part[turn.AnnotationsFile] != full[turn.AnnotationsFile] {
		t.Fatalf("partial tar %+v", part)
	}
	code, b = e.do(t, "POST", r.daemonPath("turns/"+ids+"/workspace"), r.alice.tok, turn.PathsRequest{})
	if code != http.StatusOK || len(untar(t, b)) != 0 {
		t.Fatalf("empty list: %d %d bytes", code, len(b))
	}
}

// 002-AC37: a requested path must be a file of this turn's workspace: traversal, absolute paths,
// directories and another room's files are refused, and one bad path refuses the whole list. Only
// the turn's daemon may list or fetch it.
func TestWorkspacePathsRefused(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	other := newTurnRoom(t, e)
	tn := e.press(t, r, r.alice, r.alice.p.Token, false)
	ids := strconv.FormatInt(tn.ID, 10)
	pdf := fmt.Sprintf("uploads/%d-brief.pdf", r.pdf)
	for _, p := range []string{
		"../../etc/passwd", "/etc/passwd", "uploads/../annotations.md", "./annotations.md", pdf + "/../../x",
		"docs", "docs/", "", ".", "uploads\\..\\annotations.md",
		fmt.Sprintf("uploads/%d-brief.pdf", other.pdf),
		"docs/never.md",
	} {
		code, b := e.do(t, "POST", r.daemonPath("turns/"+ids+"/workspace"), r.alice.tok, turn.PathsRequest{Paths: []string{turn.AnnotationsFile, p}})
		if code != http.StatusBadRequest {
			t.Fatalf("path %q: %d %.200s", p, code, b)
		}
	}
	bob := e.daemonFor(t, r.room, "Bob")
	e.connect(t, r.room, bob, true)
	if code, _ := e.do(t, "GET", r.daemonPath("turns/"+ids+"/manifest"), bob.tok, nil); code != http.StatusConflict {
		t.Fatalf("bob lists alice's turn: %d", code)
	}
	if code, _ := e.do(t, "POST", r.daemonPath("turns/"+ids+"/workspace"), bob.tok, turn.PathsRequest{Paths: []string{pdf}}); code != http.StatusConflict {
		t.Fatalf("bob fetches alice's turn: %d", code)
	}
	if code, _ := e.do(t, "GET", "/api/rooms/"+other.room+"/daemon/turns/"+ids+"/manifest", r.alice.tok, nil); code != http.StatusUnauthorized {
		t.Fatalf("alice's daemon on another room: %d", code)
	}
}

// 002-AC38: a full fetch that names a fallback reason is counted by reason; one without, from a
// daemon with no cache or an older one, is not.
func TestWorkspaceFallbackCounted(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tn := e.press(t, r, r.alice, r.alice.p.Token, false)
	base := r.daemonPath("turns/" + strconv.FormatInt(tn.ID, 10) + "/workspace")
	for _, q := range []string{"", "?fallback=corrupt", "?fallback=corrupt", "?fallback=missing", "?fallback=bogus"} {
		if code, b := e.do(t, "GET", base+q, r.alice.tok, nil); code != http.StatusOK || len(untar(t, b)) == 0 {
			t.Fatalf("%q: %d", q, code)
		}
	}
	got := map[string]int64{}
	for _, reason := range turn.FallbackReasons {
		got[reason] = e.srv.WorkspaceFallbacks(reason)
	}
	if got["corrupt"] != 2 || got["missing"] != 1 || got["partial"] != 0 || got["manifest"] != 0 {
		t.Fatalf("fallbacks %v", got)
	}
}
