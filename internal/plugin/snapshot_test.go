package plugin

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func reg(p, data string) Entry { return Entry{Path: p, Kind: Regular, Data: []byte(data)} }
func exe(p, data string) Entry { return Entry{Path: p, Kind: Executable, Data: []byte(data)} }
func link(p, to string) Entry  { return Entry{Path: p, Kind: Symlink, Data: []byte(to)} }
func filePaths(s *Snapshot) []string {
	var out []string
	for _, f := range s.Files {
		out = append(out, f.Path)
	}
	return out
}

// 009-AC9: the snapshot keeps the plugin's folder, symlinks inside it copied as their target.
func TestFromTree(t *testing.T) {
	tree := []Entry{
		reg("README.md", "repo"),
		reg("plugins/p/.claude-plugin/plugin.json", `{"name":"p"}`),
		exe("plugins/p/bin/tool", "#!/bin/sh\n"),
		link("plugins/p/bin/alias", "tool"),
		link("plugins/p/skills/a/SKILL.md", "../../shared/SKILL.md"),
		link("plugins/p/chain", "skills/a/SKILL.md"),
		reg("plugins/p/shared/SKILL.md", "skill"),
		reg("plugins/p/.git/config", "x"),
		reg("plugins/p/sub/.git", "gitdir: x"),
		reg("plugins/p/.GIT/config", "[core]\nfsmonitor=x\n"),
	}
	s, err := FromTree(tree, "./plugins/p/", nil, DefaultMaxBytes)
	require.NoError(t, err)
	require.Equal(t, []string{".claude-plugin/plugin.json", "bin/alias", "bin/tool", "chain", "shared/SKILL.md", "skills/a/SKILL.md"}, filePaths(s))
	require.Equal(t, "skill", string(s.data["chain"]))
	require.Equal(t, int64(len(`{"name":"p"}`)+2*len("#!/bin/sh\n")+3*len("skill")), s.Size)

	hdrs := map[string]*tar.Header{}
	tr := tar.NewReader(bytes.NewReader(s.Tar))
	for h, err := tr.Next(); err == nil; h, err = tr.Next() {
		hdrs[h.Name] = h
	}
	require.Equal(t, int64(0o755), hdrs["bin/alias"].Mode)
	require.Equal(t, int64(0o644), hdrs["chain"].Mode)
	require.Equal(t, int64(0), hdrs["chain"].ModTime.Unix())

	// The repo root, the manifest synthesized only when the plugin has none.
	s, err = FromTree(tree[:1], "", []byte(`{"name":"x"}`), DefaultMaxBytes)
	require.NoError(t, err)
	require.Equal(t, []string{".claude-plugin/plugin.json", "README.md"}, filePaths(s))
	s, err = FromTree(tree, "plugins/p", []byte(`{"name":"other"}`), DefaultMaxBytes)
	require.NoError(t, err)
	require.JSONEq(t, `{"name":"p"}`, string(s.data[ManifestPath]))
}

// 009-AC9: the same files give the same bytes, whatever order they come in.
func TestSnapshotDeterministic(t *testing.T) {
	tree := []Entry{reg("b", "2"), exe("a/x", "1"), link("c", "b")}
	s1, err := FromTree(tree, "", nil, DefaultMaxBytes)
	require.NoError(t, err)
	s2, err := FromTree([]Entry{tree[2], tree[0], tree[1]}, "", nil, DefaultMaxBytes)
	require.NoError(t, err)
	require.Equal(t, s1.SHA256, s2.SHA256)
	require.Equal(t, s1.Tar, s2.Tar)
	require.Len(t, s1.SHA256, 64)
}

// 009-AC9: each refusal names the path at fault.
func TestFromTreeRefuses(t *testing.T) {
	many := make([]Entry, MaxFiles+1)
	for i := range many {
		many[i] = reg(fmt.Sprintf("f%05d", i), "")
	}
	cases := []struct {
		name string
		tree []Entry
		root string
		max  int64
		want string
	}{
		{"leaves", []Entry{link("skills/x", "../../x"), reg("x", "")}, "", 0, "skills/x: the symlink leaves the plugin"},
		{"leaves the root", []Entry{link("p/l", "../x"), reg("x", "")}, "p", 0, "l: the symlink leaves the plugin"},
		{"absolute", []Entry{link("l", "/etc/passwd")}, "", 0, "l: the symlink is absolute"},
		{"windows absolute", []Entry{link("l", `C:\x`)}, "", 0, "l: the symlink is absolute"},
		{"dangling", []Entry{link("l", "nope")}, "", 0, "l: the symlink is dangling"},
		{"empty link", []Entry{link("l", "")}, "", 0, "l: the symlink is empty"},
		{"loop", []Entry{link("a", "b"), link("b", "a")}, "", 0, "the symlink loops"},
		{"folder", []Entry{link("l", "d"), reg("d/f", "")}, "", 0, "l: the symlink points to a folder"},
		{"submodule", []Entry{{Path: "vendor/x", Kind: Submodule}}, "", 0, "vendor/x is a git submodule"},
		{"link to submodule", []Entry{link("l", "s"), {Path: "s", Kind: Submodule}}, "", 0, "s is a git submodule"},
		{"unknown kind", []Entry{{Path: "x", Kind: 9}}, "", 0, "x is not a regular file"},
		{"case", []Entry{reg("A.md", ""), reg("a.md", "")}, "", 0, "differ only by case"},
		{"unicode form", []Entry{reg("caf\u00e9.md", ""), reg("cafe\u0301.md", "")}, "", 0, "differ only by case or Unicode form"},
		{"file and folder", []Entry{reg("a", ""), reg("a/b", "")}, "", 0, `"a" is a file and "a/b" needs it as a folder`},
		{"file and folder by case", []Entry{reg("A", ""), reg("a/b", "")}, "", 0, "needs it as a folder"},
		{"bidi", []Entry{reg("evil\u202egpj.sh", "")}, "", 0, "control character"},
		{"backslash", []Entry{reg(`a\b`, "")}, "", 0, "backslashes"},
		{"control", []Entry{reg("a\tb", "")}, "", 0, "control character"},
		{"dotdot", []Entry{reg("a/../b", "")}, "", 0, "leaves the plugin"},
		{"unclean", []Entry{reg("a//b", "")}, "", 0, "not a plain relative path"},
		{"too many", many, "", 0, "more than 5000 files"},
		{"too big", []Entry{reg("a", strings.Repeat("x", 11)), link("b", "a")}, "", 20, "bigger than 20 bytes"},
		{"bad root", []Entry{reg("x", "")}, "../x", 0, "leaves the plugin"},
		{"absolute root", []Entry{reg("x", "")}, "/x", 0, "absolute"},
		{"empty root", []Entry{reg("x", "")}, "nope", 0, `no file under "nope"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.max == 0 {
				tc.max = DefaultMaxBytes
			}
			_, err := FromTree(tc.tree, tc.root, nil, tc.max)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}
}

func symlinkOrSkip(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// 009-AC9, 009-AC20: a folder on disk snapshots like the same tree fetched, .git left out.
func TestFromDir(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".claude-plugin/plugin.json": `{"name":"p"}`, "skills/a/SKILL.md": "s", ".git/HEAD": "ref", "x/.Git": "gitdir"})
	symlinkOrSkip(t, filepath.Join("skills", "a", "SKILL.md"), filepath.Join(dir, "alias.md"))
	s, err := FromDir(dir, DefaultMaxBytes)
	require.NoError(t, err)
	require.Equal(t, []string{".claude-plugin/plugin.json", "alias.md", "skills/a/SKILL.md"}, filePaths(s))
	same, err := FromTree([]Entry{reg(".claude-plugin/plugin.json", `{"name":"p"}`), reg("skills/a/SKILL.md", "s"), link("alias.md", "skills/a/SKILL.md")}, "", nil, DefaultMaxBytes)
	require.NoError(t, err)
	require.Equal(t, same.SHA256, s.SHA256)

	if runtime.GOOS != "windows" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\n"), 0o755)) //nolint:gosec // the test's executable
		s, err = FromDir(dir, DefaultMaxBytes)
		require.NoError(t, err)
		v, err := ValidateTar(bytes.NewReader(s.Tar), DefaultMaxBytes)
		require.NoError(t, err)
		require.Equal(t, s.SHA256, v.SHA256)
	}

	_, err = FromDir(dir, 3)
	require.ErrorIs(t, err, ErrTooBig)
	_, err = FromDir(t.TempDir(), DefaultMaxBytes)
	require.ErrorContains(t, err, "empty")
	_, err = FromDir(filepath.Join(dir, "missing"), DefaultMaxBytes)
	require.Error(t, err)
}

// 009-AC9: a symlink out of the folder, absolute or dangling refuses it, naming the path.
func TestFromDirRefusesLinks(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(outside, []byte("s"), 0o600))
	for name, target := range map[string]string{
		"esc.md: the symlink leaves the plugin": filepath.Join("..", "x"),
		"esc.md: the symlink is absolute":       outside,
		"esc.md: the symlink is dangling":       "nope.md",
	} {
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{"a.md": "a"})
		symlinkOrSkip(t, target, filepath.Join(dir, "esc.md"))
		_, err := FromDir(dir, DefaultMaxBytes)
		require.ErrorContains(t, err, name)
	}
}

// 009-AC9: 5,001 files refuse a folder before all are read.
func TestFromDirTooMany(t *testing.T) {
	dir := t.TempDir()
	for i := range MaxFiles + 1 {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d", i)), nil, 0o644))
	}
	_, err := FromDir(dir, DefaultMaxBytes)
	require.ErrorContains(t, err, "more than 5000 files")
}

func tarOf(t *testing.T, hs ...*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range hs {
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		require.NoError(t, tw.WriteHeader(h))
		_, err := tw.Write(make([]byte, h.Size))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return b.Bytes()
}

// 009-AC20: an uploaded tar passes the same rules and must be normalised.
func TestValidateTar(t *testing.T) {
	s, err := FromTree([]Entry{reg("a", "1"), exe("b/c", "2")}, "", nil, DefaultMaxBytes)
	require.NoError(t, err)
	v, err := ValidateTar(bytes.NewReader(s.Tar), DefaultMaxBytes)
	require.NoError(t, err)
	require.Equal(t, s.SHA256, v.SHA256)
	require.Equal(t, s.Files, v.Files)

	many := make([]*tar.Header, MaxFiles+1)
	for i := range many {
		many[i] = &tar.Header{Name: fmt.Sprintf("f%05d", i), Mode: 0o644, ModTime: epoch}
	}
	for name, tc := range map[string]struct {
		data []byte
		max  int64
		want string
	}{
		"mtime":   {tarOf(t, &tar.Header{Name: "a", Mode: 0o644, ModTime: time.Unix(5, 0)}), 0, "normalised"},
		"symlink": {tarOf(t, &tar.Header{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "../x", ModTime: epoch}), 0, `"a" is not a regular file`},
		"mode":    {tarOf(t, &tar.Header{Name: "a", Mode: 0o4755, ModTime: epoch}), 0, "mode"},
		"dotdot":  {tarOf(t, &tar.Header{Name: "../a", Mode: 0o644, ModTime: epoch}), 0, "leaves the plugin"},
		"many":    {tarOf(t, many...), 0, "more than 5000 files"},
		"big":     {tarOf(t, &tar.Header{Name: "a", Mode: 0o644, Size: 100, ModTime: epoch}), 50, "bigger than 50 bytes"},
		"empty":   {tarOf(t), 0, "empty"},
		"garbage": {[]byte(strings.Repeat("x", 1024)), 0, "not a tar"},
	} {
		if tc.max == 0 {
			tc.max = DefaultMaxBytes
		}
		_, err := ValidateTar(bytes.NewReader(tc.data), tc.max)
		require.ErrorContains(t, err, tc.want, name)
	}
	_, err = ValidateTar(bytes.NewReader(make([]byte, 64<<20)), 1<<20)
	require.ErrorIs(t, err, ErrTooBig)
}
