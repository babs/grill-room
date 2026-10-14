package plugin

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Snapshot limits (009-AC9). DefaultMaxBytes is the instance setting's default.
const (
	DefaultMaxBytes = 20 << 20
	MaxFiles        = 5000
	maxPathLen      = 1024
	maxLinkHops     = 40
	// headerAllowance is what one tar entry's headers may take on top of its bytes.
	headerAllowance = 8 << 10
	// ManifestPath is where Claude Code reads a plugin's manifest.
	ManifestPath = ".claude-plugin/plugin.json"
)

// ErrInvalid wraps every reason a snapshot is refused; the message names the path.
var ErrInvalid = errors.New("invalid plugin")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// File is one file of a snapshot.
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Snapshot is a plugin's files as a normalised tar: sorted paths, mtime 0, owner 0, modes 0644
// or 0755, no directory entries. The same files always give the same bytes and SHA256. Size is
// the files' total bytes.
type Snapshot struct {
	Tar    []byte
	SHA256 string
	Size   int64
	Files  []File
	data   map[string][]byte
}

// FromTree snapshots the plugin rooted at root ("" or "./" for the repo root) of a fetched tree.
// manifest, when not nil, becomes .claude-plugin/plugin.json if the plugin has none (009-AC6).
func FromTree(tree []Entry, root string, manifest []byte, maxBytes int64) (*Snapshot, error) {
	prefix, err := rootPrefix(root)
	if err != nil {
		return nil, err
	}
	var es []Entry
	for _, e := range tree {
		rel, ok := strings.CutPrefix(e.Path, prefix)
		if !ok || gitPath(rel) {
			continue
		}
		e.Path = rel
		es = append(es, e)
	}
	if len(es) == 0 {
		return nil, invalid("no file under %q", root)
	}
	if manifest != nil && !slices.ContainsFunc(es, func(e Entry) bool { return e.Path == ManifestPath }) {
		es = append(es, Entry{Path: ManifestPath, Kind: Regular, Data: manifest})
	}
	return build(es, maxBytes)
}

// rootPrefix turns a plugin root into the path prefix of its files in the repo.
func rootPrefix(root string) (string, error) {
	r := path.Clean(strings.TrimPrefix(root, "./"))
	if r == "." || r == "" {
		return "", nil
	}
	if err := checkPath(r); err != nil {
		return "", err
	}
	return r + "/", nil
}

// gitPath reports whether p is or sits in a .git folder, which a snapshot leaves out. Any case:
// ".GIT" is git's own folder on a case-insensitive host.
func gitPath(p string) bool {
	return slices.ContainsFunc(strings.Split(p, "/"), func(el string) bool { return strings.EqualFold(el, ".git") })
}

// foldKey is the name two paths collide on when a macOS or Windows host unpacks them.
func foldKey(p string) string { return strings.ToLower(norm.NFC.String(p)) }

func bidi(r rune) bool { return unicode.Is(unicode.Bidi_Control, r) }

// checkPath applies the export rules: a plain relative path, nothing that could leave the folder.
func checkPath(p string) error {
	switch {
	case p == "" || len(p) > maxPathLen:
		return invalid("a path is empty or over %d characters", maxPathLen)
	case !utf8.ValidString(p) || strings.ContainsFunc(p, unicode.IsControl) || strings.ContainsFunc(p, bidi):
		return invalid("%q holds a control character or is not UTF-8", p)
	case strings.Contains(p, `\`):
		return invalid("%q uses backslashes", p)
	case path.IsAbs(p):
		return invalid("%q is an absolute path", p)
	case slices.Contains(strings.Split(p, "/"), ".."):
		return invalid("%q leaves the plugin", p)
	case path.Clean(p) != p:
		return invalid("%q is not a plain relative path", p)
	}
	return nil
}

// build checks es against the AC9 rules, replaces each symlink by the file it resolves to, and
// packs the result.
func build(es []Entry, maxBytes int64) (*Snapshot, error) {
	if len(es) > MaxFiles {
		return nil, invalid("more than %d files", MaxFiles)
	}
	byPath := make(map[string]Entry, len(es))
	folded := make(map[string]string, len(es))
	for _, e := range es {
		if err := checkPath(e.Path); err != nil {
			return nil, err
		}
		key := foldKey(e.Path)
		if other, dup := folded[key]; dup {
			return nil, invalid("%q and %q differ only by case or Unicode form", other, e.Path)
		}
		folded[key] = e.Path
		byPath[e.Path] = e
	}
	for _, e := range es {
		for d := path.Dir(e.Path); d != "."; d = path.Dir(d) {
			if other, ok := folded[foldKey(d)]; ok {
				return nil, invalid("%q is a file and %q needs it as a folder", other, e.Path)
			}
		}
	}
	files := make([]Entry, 0, len(es))
	var total int64
	for _, e := range es {
		r, err := resolve(byPath, e)
		if err != nil {
			return nil, err
		}
		if total += int64(len(r.Data)); total > maxBytes {
			return nil, tooBig("the plugin", maxBytes)
		}
		files = append(files, Entry{Path: e.Path, Kind: r.Kind, Data: r.Data})
	}
	return pack(files)
}

// resolve follows e when it is a symlink, inside the plugin only.
func resolve(byPath map[string]Entry, e Entry) (Entry, error) {
	link := e.Path
	for range maxLinkHops {
		switch e.Kind {
		case Regular, Executable:
			return e, nil
		case Submodule:
			return e, invalid("%s is a git submodule: not supported", e.Path)
		case Symlink:
		default:
			return e, invalid("%s is not a regular file", e.Path)
		}
		target := string(e.Data)
		switch {
		case target == "" || strings.ContainsRune(target, 0):
			return e, invalid("%s: the symlink is empty", link)
		case path.IsAbs(target) || strings.HasPrefix(target, `\`) || (len(target) > 1 && target[1] == ':'):
			return e, invalid("%s: the symlink is absolute", link)
		}
		t := path.Join(path.Dir(e.Path), target)
		if t == ".." || strings.HasPrefix(t, "../") {
			return e, invalid("%s: the symlink leaves the plugin", link)
		}
		next, ok := byPath[t]
		if !ok {
			// ponytail: a symlink to a folder is refused; copy the folder if plugins need it.
			for p := range byPath {
				if strings.HasPrefix(p, t+"/") {
					return e, invalid("%s: the symlink points to a folder", link)
				}
			}
			return e, invalid("%s: the symlink is dangling", link)
		}
		e = next
	}
	return e, invalid("%s: the symlink loops", link)
}

// epoch stamps every packed file, as skill.pack does.
var epoch = time.Unix(0, 0).UTC()

func pack(files []Entry) (*Snapshot, error) {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	s := &Snapshot{Files: make([]File, 0, len(files)), data: make(map[string][]byte, len(files))}
	for _, f := range files {
		mode := int64(0o644)
		if f.Kind == Executable {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.Path, Mode: mode, Size: int64(len(f.Data)), ModTime: epoch}); err != nil {
			return nil, fmt.Errorf("pack plugin: %w", err)
		}
		if _, err := tw.Write(f.Data); err != nil {
			return nil, fmt.Errorf("pack plugin: %w", err)
		}
		sum := sha256.Sum256(f.Data)
		s.Files = append(s.Files, File{Path: f.Path, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(f.Data))})
		s.Size += int64(len(f.Data))
		s.data[f.Path] = f.Data
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("pack plugin: %w", err)
	}
	s.Tar = buf.Bytes()
	sum := sha256.Sum256(s.Tar)
	s.SHA256 = hex.EncodeToString(sum[:])
	return s, nil
}

// FromDir snapshots the plugin in dir, as the host's daemon fetched it (009-AC20). Every file
// must be regular or a symlink resolving inside dir; a FIFO, socket or device refuses it.
func FromDir(dir string, maxBytes int64) (*Snapshot, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("plugin dir: %w", err)
	}
	defer root.Close() //nolint:errcheck // a directory handle: nothing to flush
	var es []Entry
	var total int64
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.EqualFold(d.Name(), ".git") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		switch t := d.Type(); {
		case t.IsDir():
			return nil
		case t&fs.ModeSymlink != 0:
			target, err := root.Readlink(p)
			if err != nil {
				return err
			}
			es = append(es, Entry{Path: p, Kind: Symlink, Data: []byte(filepath.ToSlash(target))})
		case t.IsRegular():
			e, n, err := readFile(root, p, maxBytes-total, maxBytes)
			if err != nil {
				return err
			}
			total += n
			es = append(es, e)
		default:
			return invalid("%s is not a regular file", p)
		}
		if len(es) > MaxFiles {
			return invalid("more than %d files", MaxFiles)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrInvalid) || errors.Is(err, ErrTooBig) {
			return nil, err
		}
		return nil, fmt.Errorf("plugin dir: %w", err)
	}
	if len(es) == 0 {
		return nil, invalid("the folder is empty")
	}
	return build(es, maxBytes)
}

func readFile(root *os.Root, p string, left, maxBytes int64) (Entry, int64, error) {
	f, err := root.Open(p)
	if err != nil {
		return Entry{}, 0, err
	}
	defer f.Close() //nolint:errcheck // read only
	st, err := f.Stat()
	if err != nil {
		return Entry{}, 0, err
	}
	if !st.Mode().IsRegular() {
		return Entry{}, 0, invalid("%s is not a regular file", p)
	}
	data, err := io.ReadAll(io.LimitReader(f, left+1))
	if err != nil {
		return Entry{}, 0, err
	}
	if int64(len(data)) > left {
		return Entry{}, 0, tooBig("the plugin", maxBytes)
	}
	k := Regular
	if st.Mode()&0o111 != 0 {
		k = Executable
	}
	return Entry{Path: p, Kind: k, Data: data}, int64(len(data)), nil
}

// MaxTarBytes is the largest tar of a snapshot whose files hold at most maxBytes.
func MaxTarBytes(maxBytes int64) int64 { return maxBytes + int64(MaxFiles+2)*headerAllowance }

// ValidateTar checks a snapshot tar uploaded by a daemon or carried by an import against the
// rules FromTree applies, and requires it normalised so its SHA256 is the snapshot's.
func ValidateTar(r io.Reader, maxBytes int64) (*Snapshot, error) {
	limit := MaxTarBytes(maxBytes)
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("plugin tar: %w", err)
	}
	if int64(len(raw)) > limit {
		return nil, tooBig("the plugin", maxBytes)
	}
	tr := tar.NewReader(bytes.NewReader(raw))
	var es []Entry
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, invalid("not a tar: %v", err)
		}
		if h.Typeflag != tar.TypeReg {
			return nil, invalid("%q is not a regular file", h.Name)
		}
		k := Regular
		switch h.Mode {
		case 0o644:
		case 0o755:
			k = Executable
		default:
			return nil, invalid("%q has mode %o", h.Name, h.Mode)
		}
		if len(es) == MaxFiles {
			return nil, invalid("more than %d files", MaxFiles)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, invalid("%q: %v", h.Name, err)
		}
		es = append(es, Entry{Path: h.Name, Kind: k, Data: data})
	}
	if len(es) == 0 {
		return nil, invalid("the tar is empty")
	}
	s, err := build(es, maxBytes)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(s.Tar, raw) {
		return nil, invalid("the tar is not in normalised form")
	}
	return s, nil
}
