package skill

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
)

type entry struct {
	name string
	body string
	mode fs.FileMode
}

func mkzip(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const good = "---\nname: hello-stamp\ndescription: Stamp a document. Use when asked for a stamp.\n---\nRun `bash scripts/stamp.sh`.\n"

func untar(t *testing.T, b []byte) map[string]*tar.Header {
	t.Helper()
	out := map[string]*tar.Header{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = h
	}
}

// 002-AC29: a skill folder at the zip root, or in its single top folder, parses; Finder's litter
// and directory entries are dropped; the exec bit survives; the same files give the same hash.
func TestParseGood(t *testing.T) {
	root := mkzip(t, entry{name: "SKILL.md", body: good}, entry{name: "scripts/", mode: fs.ModeDir | 0o755},
		entry{name: "scripts/stamp.sh", body: "echo ok\n", mode: 0o755}, entry{name: ".DS_Store", body: "x"})
	nested := mkzip(t, entry{name: "hello-stamp/SKILL.md", body: good}, entry{name: "hello-stamp/scripts/stamp.sh", body: "echo ok\n", mode: 0o755},
		entry{name: "__MACOSX/hello-stamp/._SKILL.md", body: "x"})
	var sums []string
	for name, z := range map[string][]byte{"root": root, "nested": nested} {
		t.Run(name, func(t *testing.T) {
			sk, err := Parse(z)
			if err != nil {
				t.Fatal(err)
			}
			if sk.Name != "hello-stamp" || sk.Description != "Stamp a document. Use when asked for a stamp." || sk.Files != 2 {
				t.Fatalf("parsed %+v", sk)
			}
			files := untar(t, sk.Tar)
			if len(files) != 2 || files["SKILL.md"].Mode != 0o644 || files["scripts/stamp.sh"].Mode != 0o755 {
				t.Fatalf("packed %v", files)
			}
			sums = append(sums, sk.SHA256)
		})
	}
	if len(sums) != 2 || sums[0] != sums[1] {
		t.Fatalf("same files, different hashes: %v", sums)
	}
}

// 002-AC29: hostile and broken zips are refused, each for its own reason.
func TestParseRefuses(t *testing.T) {
	big := strings.Repeat("0", MaxTotalBytes+1)
	many := []entry{{name: "SKILL.md", body: good}}
	for i := range MaxFiles {
		many = append(many, entry{name: "f/" + strings.Repeat("a", i%50+1) + string(rune('a'+i/50)), body: "x"})
	}
	cases := map[string]struct {
		zip  []byte
		want string
	}{
		"not a zip":              {[]byte("PK nope"), "not a zip"},
		"empty":                  {mkzip(t), "empty"},
		"zip slip":               {mkzip(t, entry{name: "SKILL.md", body: good}, entry{name: "../evil.sh", body: "x"}), "leaves the skill folder"},
		"zip slip deep":          {mkzip(t, entry{name: "SKILL.md", body: good}, entry{name: "a/../../evil.sh", body: "x"}), "leaves the skill folder"},
		"absolute":               {mkzip(t, entry{name: "SKILL.md", body: good}, entry{name: "/etc/cron.d/x", body: "x"}), "absolute"},
		"backslash":              {mkzip(t, entry{name: "SKILL.md", body: good}, entry{name: `..\evil`, body: "x"}), "backslashes"},
		"not canonical":          {mkzip(t, entry{name: "SKILL.md", body: good}, entry{name: "a//b", body: "x"}), "plain relative"},
		"dot path":               {mkzip(t, entry{name: "SKILL.md", body: good}, entry{name: "./b", body: "x"}), "plain relative"},
		"symlink":                {mkzip(t, entry{name: "SKILL.md", body: good}, entry{name: "creds", body: "/home/agent/.claude/.credentials.json", mode: fs.ModeSymlink | 0o777}), "symlink"},
		"symlinked SKILL.md":     {mkzip(t, entry{name: "SKILL.md", body: "/etc/passwd", mode: fs.ModeSymlink | 0o777}), "symlink"},
		"device":                 {mkzip(t, entry{name: "SKILL.md", body: good}, entry{name: "dev", mode: fs.ModeDevice | 0o644}), "not a regular file"},
		"control char":           {mkzip(t, entry{name: "SKILL.md", body: good}, entry{name: "a\x1bb", body: "x"}), "control character"},
		"duplicate":              {mkzip(t, entry{name: "SKILL.md", body: good}, entry{name: "a.txt", body: "x"}, entry{name: "A.txt", body: "y"}), "twice"},
		"oversize":               {mkzip(t, entry{name: "SKILL.md", body: good}, entry{name: "blob", body: big}), "once unpacked"},
		"oversize split":         {mkzip(t, entry{name: "SKILL.md", body: good}, entry{name: "a", body: big[:MaxTotalBytes/2+1]}, entry{name: "b", body: big[:MaxTotalBytes/2+1]}), "once unpacked"},
		"too many files":         {mkzip(t, many...), "more than"},
		"no SKILL.md":            {mkzip(t, entry{name: "README.md", body: "hi"}), "no SKILL.md"},
		"two top folders":        {mkzip(t, entry{name: "a/SKILL.md", body: good}, entry{name: "b/x", body: "x"}), "no SKILL.md"},
		"SKILL.md too deep":      {mkzip(t, entry{name: "a/b/SKILL.md", body: good}), "no SKILL.md"},
		"missing frontmatter":    {mkzip(t, entry{name: "SKILL.md", body: "# Hello\n"}), "must start with"},
		"unclosed frontmatter":   {mkzip(t, entry{name: "SKILL.md", body: "---\nname: a\n"}), "no closing"},
		"bad yaml":               {mkzip(t, entry{name: "SKILL.md", body: "---\nname: [a\n---\n"}), "frontmatter:"},
		"no name":                {mkzip(t, entry{name: "SKILL.md", body: "---\ndescription: d\n---\n"}), "name must be"},
		"uppercase name":         {mkzip(t, entry{name: "SKILL.md", body: "---\nname: Hello\ndescription: d\n---\n"}), "name must be"},
		"path in name":           {mkzip(t, entry{name: "SKILL.md", body: "---\nname: ../x\ndescription: d\n---\n"}), "name must be"},
		"no description":         {mkzip(t, entry{name: "SKILL.md", body: "---\nname: a\n---\n"}), "description must be"},
		"description too long":   {mkzip(t, entry{name: "SKILL.md", body: "---\nname: a\ndescription: " + strings.Repeat("d", MaxDescription+1) + "\n---\n"}), "description must be"},
		"SKILL.md too large":     {mkzip(t, entry{name: "SKILL.md", body: good + strings.Repeat("x", MaxSkillMD)}), "SKILL.md is over"},
		"zip itself too large":   {make([]byte, MaxZipBytes+1), "zip is over"},
		"path too long":          {mkzip(t, entry{name: "SKILL.md", body: good}, entry{name: strings.Repeat("a", MaxPathLen+1), body: "x"}), "over 200 characters"},
		"invalid utf8 path":      {mkzip(t, entry{name: "SKILL.md", body: good}, entry{name: "a\xffb", body: "x"}), "not UTF-8"},
		"slip after top folder":  {mkzip(t, entry{name: "x/SKILL.md", body: good}, entry{name: "x/../../y", body: "x"}), "leaves the skill folder"},
		"lying header oversized": {lyingZip(t), `"blob"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sk, err := Parse(tc.zip)
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %+v, %v; want ErrInvalid with %q", sk, err, tc.want)
			}
		})
	}
}

// lyingZip holds a zip bomb whose header claims a few bytes: it must be refused, not unpacked on
// the header's word.
func lyingZip(t *testing.T) []byte {
	t.Helper()
	var payload bytes.Buffer
	pw := zip.NewWriter(&payload)
	w, _ := pw.Create("blob")
	_, _ = w.Write(bytes.Repeat([]byte{0}, MaxTotalBytes+10))
	_ = pw.Close()
	zr, err := zip.NewReader(bytes.NewReader(payload.Bytes()), int64(payload.Len()))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := zr.File[0].OpenRaw()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	sw, _ := zw.Create("SKILL.md")
	_, _ = io.WriteString(sw, good)
	h := zr.File[0].FileHeader
	h.UncompressedSize64 = 10
	rw, err := zw.CreateRaw(&h)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(rw, raw)
	_ = zw.Close()
	return buf.Bytes()
}

// 002-AC31: the plugin directory claude --plugin-dir loads: its manifest, and each skill re-rooted
// under skills/<name>/ with its folders and modes.
func TestWritePlugin(t *testing.T) {
	sk, err := Parse(mkzip(t, entry{name: "SKILL.md", body: good}, entry{name: "scripts/lib/stamp.sh", body: "echo ok\n", mode: 0o755}))
	if err != nil {
		t.Fatal(err)
	}
	other, err := Parse(mkzip(t, entry{name: "SKILL.md", body: strings.Replace(good, "hello-stamp", "other", 1)}))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WritePlugin(&buf, []Packed{{Name: sk.Name, Tar: sk.Tar}, {Name: other.Name, Tar: other.Tar}}); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	got := map[string]int64{}
	var manifest []byte
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got[h.Name] = h.Mode
		if h.Name == ".claude-plugin/plugin.json" {
			manifest, _ = io.ReadAll(tr)
		}
	}
	for name, mode := range map[string]int64{
		".claude-plugin/": 0o755, ".claude-plugin/plugin.json": 0o644, "skills/": 0o755,
		"skills/hello-stamp/": 0o755, "skills/hello-stamp/SKILL.md": 0o644, "skills/hello-stamp/scripts/": 0o755,
		"skills/hello-stamp/scripts/lib/": 0o755, "skills/hello-stamp/scripts/lib/stamp.sh": 0o755,
		"skills/other/": 0o755, "skills/other/SKILL.md": 0o644,
	} {
		if got[name] != mode {
			t.Errorf("%s: mode %o, want %o (all: %v)", name, got[name], mode, got)
		}
	}
	var m map[string]string
	if err := json.Unmarshal(manifest, &m); err != nil || m["name"] != "grill-room" {
		t.Fatalf("manifest %s: %v", manifest, err)
	}
	if err := WritePlugin(io.Discard, []Packed{{Name: "../x", Tar: sk.Tar}}); err == nil {
		t.Fatal("a bad name reached the plugin")
	}
}

// tarOf writes headers with bodies as a tar, as a hand-made archive would carry a skill.
func tarOf(t *testing.T, files map[*tar.Header]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for h, body := range files {
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(tw, body)
	}
	_ = tw.Close()
	return buf.Bytes()
}

// 004-AC14: an imported skill version passes the upload rules again, and must be the normalised
// tar the store keeps, unchanged.
func TestParseTar(t *testing.T) {
	sk, err := Parse(mkzip(t, entry{name: "SKILL.md", body: good}, entry{name: "scripts/stamp.sh", body: "echo ok\n", mode: 0o755}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseTar(sk.Tar)
	if err != nil || got.SHA256 != sk.SHA256 || got.Name != "hello-stamp" || got.Files != 2 {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for name, data := range map[string][]byte{
		"not normalised": tarOf(t, map[*tar.Header]string{{Name: "SKILL.md", Mode: 0o600}: good}),
		"trailing bytes": append(append([]byte{}, sk.Tar...), 0),
		"symlink":        tarOf(t, map[*tar.Header]string{{Name: "SKILL.md", Mode: 0o644}: good, {Name: "x", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}: ""}),
		"traversal":      tarOf(t, map[*tar.Header]string{{Name: "SKILL.md", Mode: 0o644}: good, {Name: "../x", Mode: 0o644}: "x"}),
		"no SKILL.md":    tarOf(t, map[*tar.Header]string{{Name: "README.md", Mode: 0o644}: "x"}),
		"not a tar":      []byte("plain text, long enough to be read as a broken tar header block by the reader"),
		"too large":      make([]byte, MaxZipBytes+1),
	} {
		if _, err := ParseTar(data); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
