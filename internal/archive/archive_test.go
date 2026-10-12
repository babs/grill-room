package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var limits = Limits{MaxBytes: 1 << 20, MaxFiles: 100}

// sample writes a small export directory.
func sample(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"room.json": `{"name":"R"}`, "messages.jsonl": "{}\n", "uploads/1": "data", "documents/2/1.md": "# Doc"} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func pack(t *testing.T, dir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := Manifest{InstanceVersion: "v1.2.3", InstanceID: "inst", CreatedAt: time.Unix(1, 0).UTC(), Room: Room{ID: "r", Name: "R"}}
	if err := Pack(&buf, dir, m); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// entry is one entry of a hand-made archive.
type entry struct {
	h    tar.Header
	body string
}

// craft writes entries, then a manifest listing the regular ones with their real hashes, edited by
// edit when set.
func craft(t *testing.T, entries []entry, edit func(*Manifest)) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	m := Manifest{Format: Format, Files: map[string]File{}}
	for _, e := range entries {
		h := e.h
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		h.Size = int64(len(e.body))
		if h.Typeflag != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
		if h.Typeflag == tar.TypeReg {
			m.Files[h.Name] = fileOf([]byte(e.body))
		}
	}
	if edit != nil {
		edit(&m)
	}
	b, _ := json.Marshal(m)
	if err := tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o644, Size: int64(len(b))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func fileOf(b []byte) File {
	h := sha256.Sum256(b)
	return File{Size: int64(len(b)), SHA256: hex.EncodeToString(h[:])}
}

// emptyTar is a gzip'd tar with no entry.
func emptyTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := tar.NewWriter(gz).Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// 004-AC11 004-AC14: what Pack writes, Unpack restores whole, with its manifest.
func TestPackUnpack(t *testing.T) {
	src := sample(t)
	b := pack(t, src)
	dst := t.TempDir()
	m, err := Unpack(bytes.NewReader(b), dst, limits)
	if err != nil {
		t.Fatal(err)
	}
	if m.Format != Format || m.InstanceVersion != "v1.2.3" || m.Room.Name != "R" || len(m.Files) != 4 || m.Bytes() != int64(len(`{"name":"R"}`)+3+4+5) {
		t.Fatalf("manifest %+v", m)
	}
	got, err := os.ReadFile(filepath.Join(dst, "documents", "2", "1.md"))
	if err != nil || string(got) != "# Doc" {
		t.Fatalf("documents/2/1.md: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dst, ManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the manifest was extracted: %v", err)
	}
}

// 004-AC14: each archive an instance must refuse is refused with its reason: tampered, newer
// format, unsafe paths and entry types, too large or too many once uncompressed, incomplete.
func TestUnpackRefusals(t *testing.T) {
	good := []entry{{tar.Header{Name: "room.json", Mode: 0o644}, `{}`}}
	// A file changed after export: same size, another sha256 than the manifest's.
	tampered := craft(t, []entry{{tar.Header{Name: "uploads/1", Mode: 0o644}, "DATA"}}, func(m *Manifest) { m.Files["uploads/1"] = fileOf([]byte("data")) })
	for name, c := range map[string]struct {
		archive []byte
		l       Limits
		want    string
	}{
		"tampered file":   {tampered, limits, "sha256 mismatch on uploads/1"},
		"newer format":    {craft(t, good, func(m *Manifest) { m.Format = Format + 1 }), limits, fmt.Sprintf("format %d is newer than this instance reads (%d)", Format+1, Format)},
		"no format":       {craft(t, good, func(m *Manifest) { m.Format = 0 }), limits, "format 0 is not a format"},
		"traversal":       {craft(t, []entry{{tar.Header{Name: "../evil", Mode: 0o644}, "x"}}, nil), limits, `unsafe path "../evil"`},
		"inner traversal": {craft(t, []entry{{tar.Header{Name: "uploads/../../evil", Mode: 0o644}, "x"}}, nil), limits, "unsafe path"},
		"absolute path":   {craft(t, []entry{{tar.Header{Name: "/etc/passwd", Mode: 0o644}, "x"}}, nil), limits, `unsafe path "/etc/passwd"`},
		"dot path":        {craft(t, []entry{{tar.Header{Name: "./room.json", Mode: 0o644}, "x"}}, nil), limits, "unsafe path"},
		"backslash":       {craft(t, []entry{{tar.Header{Name: `uploads\..\x`, Mode: 0o644}, "x"}}, nil), limits, "unsafe path"},
		"symlink":         {craft(t, []entry{{tar.Header{Name: "uploads/1", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}, ""}}, nil), limits, "a symlink"},
		"hard link":       {craft(t, []entry{{tar.Header{Name: "uploads/1", Typeflag: tar.TypeLink, Linkname: "room.json"}, ""}}, nil), limits, "a hard link"},
		"device":          {craft(t, []entry{{tar.Header{Name: "dev", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}, ""}}, nil), limits, "a device"},
		"fifo":            {craft(t, []entry{{tar.Header{Name: "fifo", Typeflag: tar.TypeFifo}, ""}}, nil), limits, "a FIFO"},
		"directory":       {craft(t, []entry{{tar.Header{Name: "uploads/", Typeflag: tar.TypeDir}, ""}}, nil), limits, "a directory"},
		"bomb":            {craft(t, []entry{{tar.Header{Name: "big", Mode: 0o644}, strings.Repeat("0", 2<<20)}}, nil), limits, "over 1 MB once uncompressed"},
		"bomb in pieces": {craft(t, []entry{{tar.Header{Name: "a", Mode: 0o644}, strings.Repeat("0", 600<<10)}, {tar.Header{Name: "b", Mode: 0o644}, strings.Repeat("0", 600<<10)}}, nil),
			limits, "once uncompressed"},
		"too many files": {craft(t, []entry{{tar.Header{Name: "a", Mode: 0o644}, ""}, {tar.Header{Name: "b", Mode: 0o644}, ""}, {tar.Header{Name: "c", Mode: 0o644}, ""}}, nil),
			Limits{MaxBytes: 1 << 20, MaxFiles: 2}, "more than 2 files"},
		"duplicate":     {craft(t, []entry{{tar.Header{Name: "a", Mode: 0o644}, "1"}, {tar.Header{Name: "a", Mode: 0o644}, "2"}}, nil), limits, `"a" is in the archive twice`},
		"unlisted file": {craft(t, good, func(m *Manifest) { delete(m.Files, "room.json") }), limits, "room.json is not in the manifest"},
		"missing file":  {craft(t, good, func(m *Manifest) { m.Files["uploads/9"] = File{Size: 1, SHA256: "x"} }), limits, "uploads/9 is in the manifest but not"},
		"not gzip":      {[]byte("plain text"), limits, "not a gzip archive"},
		"no manifest":   {emptyTar(t), limits, "no manifest.json"},
		"two manifests": {craft(t, []entry{{tar.Header{Name: ManifestName, Mode: 0o644}, "nope"}}, nil), limits, "twice"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := Unpack(bytes.NewReader(c.archive), dir, c.l)
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v, want %q", err, c.want)
			}
			// Nothing ever lands outside the directory.
			if _, err := os.Lstat(filepath.Join(filepath.Dir(dir), "evil")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("an entry escaped: %v", err)
			}
		})
	}
}

type failWriter struct{ left int }

func (w *failWriter) Write(p []byte) (int, error) {
	if len(p) > w.left {
		return 0, errors.New("disk full")
	}
	w.left -= len(p)
	return len(p), nil
}

// 004-AC11: Pack fails on a directory it cannot read, a file name the format does not allow, and a
// writer that stops taking bytes.
func TestPackFailures(t *testing.T) {
	m := Manifest{CreatedAt: time.Unix(1, 0)}
	if err := Pack(&bytes.Buffer{}, filepath.Join(t.TempDir(), "missing"), m); err == nil {
		t.Error("missing directory: no error")
	}
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "two words"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Pack(&bytes.Buffer{}, bad, m); err == nil || !strings.Contains(err.Error(), "not a valid archive path") {
		t.Errorf("bad name: %v", err)
	}
	big := t.TempDir()
	if err := os.WriteFile(filepath.Join(big, "data"), bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7}, 300_000), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 100, 4096} {
		if err := Pack(&failWriter{left: n}, big, m); err == nil {
			t.Errorf("writer failing after %d bytes: no error", n)
		}
	}
}

// rawHeader is a hand-made ustar header block, of a type tar.Writer would not write as is.
func rawHeader(name string, typ byte, size int) []byte {
	h := make([]byte, 512)
	copy(h, name)
	copy(h[100:], "0000644\x00")
	copy(h[108:], "0000000\x00")
	copy(h[116:], "0000000\x00")
	copy(h[124:], fmt.Sprintf("%011o\x00", size))
	copy(h[136:], "00000000000\x00")
	h[156] = typ
	copy(h[257:], "ustar  \x00")
	copy(h[148:], "        ")
	sum := 0
	for _, c := range h {
		sum += int(c)
	}
	copy(h[148:], fmt.Sprintf("%06o\x00 ", sum))
	return h
}

// 004-AC14: headers alone cannot inflate past the cap, and a file named like a directory of
// another is the archive's fault, refused.
func TestUnpackHeadersAndClashes(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	body := bytes.Repeat([]byte{'a'}, 512<<10)
	// GNU long-name records, one after the other: tar reads them all before the entry they name.
	for range 100 {
		_, _ = gz.Write(rawHeader("././@LongLink", 'L', len(body)))
		_, _ = gz.Write(body)
	}
	_ = gz.Close()
	if _, err := Unpack(bytes.NewReader(buf.Bytes()), t.TempDir(), Limits{MaxBytes: 1 << 20, MaxFiles: 10}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "once uncompressed") {
		t.Errorf("long-name records: %v", err)
	}
	clash := craft(t, []entry{{tar.Header{Name: "a", Mode: 0o644}, "x"}, {tar.Header{Name: "a/b", Mode: 0o644}, "y"}}, nil)
	if _, err := Unpack(bytes.NewReader(clash), t.TempDir(), limits); !errors.Is(err, ErrRefused) {
		t.Errorf("file and directory of one name: %v", err)
	}
}
