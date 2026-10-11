package daemon

import (
	"archive/tar"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/babs/grill-room/internal/turn"
)

// wsCache keeps workspace files per room, content-addressed by sha256, in the daemon's state dir
// and out of every sandbox (002-AC37). One lock covers a turn's whole use of it, from the check
// to the end of the tar it streams, so an eviction never removes a file a turn still reads.
// ponytail: one lock for every room; a daemon serves one room at a time.
type wsCache struct {
	dir string
	max int64
	mu  sync.Mutex
}

var (
	errCorrupt = errors.New("a cached workspace file failed its sha256")
	sha256Re   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// roomDir hashes the room URL: it holds characters a file name may not.
func (c *wsCache) roomDir(r Room) string {
	sum := sha256.Sum256([]byte(r.String()))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:16]))
}

// forget removes a room's cached files, once the room revoked this daemon or was deleted.
func (c *wsCache) forget(r Room) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return os.RemoveAll(c.roomDir(r))
}

// validManifest refuses a manifest whose paths could leave /workspace or clash, or whose digests
// could not name a cache file.
func validManifest(m *turn.Manifest) error {
	seen := map[string]bool{}
	for _, d := range m.Dirs {
		if !turn.ValidPath(d) || seen[d] {
			return fmt.Errorf("manifest: bad dir %q", d)
		}
		seen[d] = true
	}
	for _, f := range m.Files {
		if !turn.ValidPath(f.Path) || seen[f.Path] || !sha256Re.MatchString(f.SHA256) || f.Size < 0 || f.Mode&^0o777 != 0 {
			return fmt.Errorf("manifest: bad file %q", f.Path)
		}
		seen[f.Path] = true
	}
	return nil
}

// missing returns the manifest files absent from dir. A cached file that fails its hash is removed
// and fails the check: the caller fetches the full workspace (002-AC38). Present files are touched,
// so eviction drops the least recently used first.
func missing(dir string, files []turn.File) ([]turn.File, error) {
	var out []turn.File
	now := time.Now()
	for _, f := range files {
		p := filepath.Join(dir, f.SHA256)
		ok, err := intact(p, f)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			out = append(out, f)
		case err != nil:
			return nil, err
		case !ok:
			_ = os.Remove(p)
			return nil, fmt.Errorf("%s: %w", f.Path, errCorrupt)
		default:
			_ = os.Chtimes(p, now, now)
		}
	}
	return out, nil
}

func intact(p string, f turn.File) (bool, error) {
	fh, err := os.Open(p) //nolint:gosec // p is the cache dir joined with a validated hex digest
	if err != nil {
		return false, err
	}
	defer fh.Close() //nolint:errcheck // read only
	h := sha256.New()
	n, err := io.Copy(h, fh)
	if err != nil {
		return false, fmt.Errorf("read cache: %w", err)
	}
	return n == f.Size && hex.EncodeToString(h.Sum(nil)) == f.SHA256, nil
}

// ingest stores the tar's files in dir under their sha256 and returns its dirs and files. With
// want set, the tar may hold only those files, each with the manifest's digest, and all of them.
func ingest(dir string, r io.Reader, want map[string]turn.File) (dirs []string, files []turn.File, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create cache dir: %w", err)
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read workspace tar: %w", err)
		}
		switch {
		case h.Typeflag == tar.TypeDir && want == nil && turn.ValidPath(strings.TrimSuffix(h.Name, "/")):
			dirs = append(dirs, strings.TrimSuffix(h.Name, "/"))
			continue
		case h.Typeflag != tar.TypeReg || !turn.ValidPath(h.Name):
			return nil, nil, fmt.Errorf("workspace tar: refused entry %q", h.Name)
		}
		w, asked := want[h.Name]
		if want != nil && !asked {
			return nil, nil, fmt.Errorf("workspace tar: %q was not requested", h.Name)
		}
		f, err := storeFile(dir, h.Name, h.Mode&0o777, tr)
		if err != nil {
			return nil, nil, err
		}
		if want != nil {
			if w.SHA256 != f.SHA256 || w.Size != f.Size {
				return nil, nil, fmt.Errorf("workspace tar: %q differs from the manifest", f.Path)
			}
			delete(want, f.Path)
		}
		files = append(files, f)
	}
	if len(want) > 0 {
		return nil, nil, fmt.Errorf("workspace tar: %d requested files missing", len(want))
	}
	return dirs, files, nil
}

// storeFile writes one file to dir under its sha256, through a rename: a cut transfer leaves no entry.
func storeFile(dir, name string, mode int64, r io.Reader) (turn.File, error) {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return turn.File{}, fmt.Errorf("cache: %w", err)
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return turn.File{}, fmt.Errorf("cache %s: %w", name, err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if err := os.Rename(tmp.Name(), filepath.Join(dir, sum)); err != nil {
		return turn.File{}, fmt.Errorf("cache %s: %w", name, err)
	}
	return turn.File{Path: name, SHA256: sum, Size: n, Mode: mode}, nil
}

// assemble streams the workspace tar from dir's files; done runs once the tar is written or its
// reader closed.
func assemble(dir string, dirs []string, files []turn.File, done func()) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer done()
		pw.CloseWithError(writeTar(pw, dir, dirs, files))
	}()
	return pr
}

func writeTar(w io.Writer, dir string, dirs []string, files []turn.File) error {
	tw := tar.NewWriter(w)
	now := time.Now()
	for _, d := range dirs {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: 0o755, ModTime: now}); err != nil {
			return err
		}
	}
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.Path, Mode: f.Mode & 0o777, Size: f.Size, ModTime: now}); err != nil {
			return err
		}
		fh, err := os.Open(filepath.Join(dir, f.SHA256)) //nolint:gosec // a validated hex digest in the cache dir
		if err != nil {
			return fmt.Errorf("cache: %w", err)
		}
		_, err = io.Copy(tw, fh)
		_ = fh.Close()
		if err != nil {
			return fmt.Errorf("cache %s: %w", f.Path, err)
		}
	}
	return tw.Close()
}

// evict removes the least recently used files, every room together, until the cache fits its cap.
// The caller holds mu.
func (c *wsCache) evict() {
	type entry struct {
		path string
		size int64
		used time.Time
	}
	var all []entry
	var total int64
	_ = filepath.WalkDir(c.dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal to the turn
		}
		info, err := e.Info()
		if err != nil {
			return nil //nolint:nilerr // gone since the listing
		}
		all = append(all, entry{p, info.Size(), info.ModTime()})
		total += info.Size()
		return nil
	})
	if total <= c.max {
		return
	}
	slices.SortFunc(all, func(a, b entry) int { return cmp.Compare(a.used.UnixNano(), b.used.UnixNano()) })
	for _, e := range all {
		if total <= c.max {
			return
		}
		if os.Remove(e.path) == nil {
			total -= e.size
		}
	}
}
