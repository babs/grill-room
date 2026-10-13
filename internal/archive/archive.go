// Package archive packs a directory of room export files into the versioned tar.gz a room
// exports to, and unpacks one safely: plain relative paths, regular files only, bounded sizes and
// counts, and every file checked against the sha256 its manifest lists (spec 004, AC11, AC14).
// The layout of the files is the store's; docs/export-format.md describes both.
package archive

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Format is the archive format this code writes, and the newest it reads. 2 adds the mention
// chips and the deleted documents (005-AC4, 005-AC26); 1 still imports.
const Format = 2

// ManifestName is the manifest's path in the archive: its last entry.
const ManifestName = "manifest.json"

// maxManifest bounds the manifest, which is read into memory.
const maxManifest = 16 << 20

// headerAllowance is what one entry's headers may take on top of its bytes once uncompressed.
const headerAllowance = 8 << 10

// capped stops reading past left bytes: a decompression bomb ends there whatever it is made of.
type capped struct {
	r         io.Reader
	left, max int64
}

func (c *capped) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, refuse("over %s once uncompressed", size(c.max))
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// ErrRefused wraps every reason an archive is refused; the message says which.
var ErrRefused = errors.New("archive refused")

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// unreadable refuses an archive that cannot be read, keeping err: a body cut at its size limit
// stays an http.MaxBytesError to the caller.
func unreadable(what string, err error) error {
	if errors.Is(err, ErrRefused) {
		return err
	}
	return fmt.Errorf("%w: %s: %w", ErrRefused, what, err)
}

// File is one file of an archive, as the manifest lists it.
type File struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Room names the room an archive holds.
type Room struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Manifest describes an archive: its format, the instance that wrote it, the room, and every
// other file with its size and sha256.
type Manifest struct {
	Format          int             `json:"format"`
	InstanceVersion string          `json:"instance_version"`
	InstanceID      string          `json:"instance_id"`
	CreatedAt       time.Time       `json:"created_at"`
	Room            Room            `json:"room"`
	Files           map[string]File `json:"files"`
}

// Bytes is the total size of the files the manifest lists.
func (m *Manifest) Bytes() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
}

// nameRe is what a path in an archive may be: the store writes nothing else.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*$`)

// validName reports whether name is a plain relative path this format uses.
func validName(name string) bool {
	return len(name) <= 255 && nameRe.MatchString(name) && path.Clean(name) == name && !slices.Contains(strings.Split(name, "/"), "..")
}

// Pack writes dir's files as a tar.gz to w, in path order, then m with every file's size and
// sha256, computed as they stream. Pack reads each file once and holds none in memory.
func Pack(w io.Writer, dir string, m Manifest) error {
	var names []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		names = append(names, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return fmt.Errorf("pack: %w", err)
	}
	gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return fmt.Errorf("pack: %w", err)
	}
	tw := tar.NewWriter(gz)
	m.Format, m.Files = Format, make(map[string]File, len(names))
	for _, name := range names {
		if !validName(name) || name == ManifestName {
			return fmt.Errorf("pack: %q is not a valid archive path", name)
		}
		f, err := packFile(tw, filepath.Join(dir, filepath.FromSlash(name)), name, m.CreatedAt)
		if err != nil {
			return err
		}
		m.Files[name] = f
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("pack manifest: %w", err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o644, Size: int64(len(b)), ModTime: m.CreatedAt}); err != nil {
		return fmt.Errorf("pack manifest: %w", err)
	}
	if _, err := tw.Write(b); err != nil {
		return fmt.Errorf("pack manifest: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("pack: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("pack: %w", err)
	}
	return nil
}

func packFile(tw *tar.Writer, p, name string, at time.Time) (File, error) {
	f, err := os.Open(p) //nolint:gosec // a file of the export's own staging directory
	if err != nil {
		return File{}, fmt.Errorf("pack %s: %w", name, err)
	}
	defer f.Close() //nolint:errcheck // read only
	st, err := f.Stat()
	if err != nil {
		return File{}, fmt.Errorf("pack %s: %w", name, err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: st.Size(), ModTime: at}); err != nil {
		return File{}, fmt.Errorf("pack %s: %w", name, err)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tw, h), f); err != nil {
		return File{}, fmt.Errorf("pack %s: %w", name, err)
	}
	return File{Size: st.Size(), SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// Limits bound what Unpack extracts, whatever the archive's headers claim.
type Limits struct {
	// MaxBytes bounds the files' total size once uncompressed; MaxFiles, their number.
	MaxBytes int64
	MaxFiles int
}

// Unpack extracts the tar.gz r into dir, which must exist and be empty, and returns its manifest
// once every file matches it. It refuses, with ErrRefused and the reason, anything but a gzip'd
// tar of regular files under plain relative paths, more than l allows, a format newer than Format,
// and any file missing from the manifest, absent, or of another size or sha256. On a refusal dir
// holds what was extracted so far: the caller removes it.
func Unpack(r io.Reader, dir string, l Limits) (*Manifest, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("unpack: %w", err)
	}
	defer root.Close() //nolint:errcheck // a directory handle: nothing to flush
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, unreadable("not a gzip archive", err)
	}
	// Headers count too: GNU long names and PAX records inflate without being a file's bytes.
	tr := tar.NewReader(&capped{r: gz, left: l.MaxBytes + int64(l.MaxFiles+2)*headerAllowance + maxManifest, max: l.MaxBytes})
	got := map[string]File{}
	var manifest []byte
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, unreadable("not a tar archive", err)
		}
		if h.Typeflag != tar.TypeReg {
			return nil, refuse("unsafe entry %q: only regular files are allowed, not %s", h.Name, entryKind(h.Typeflag))
		}
		if !validName(h.Name) {
			return nil, refuse("unsafe path %q: only plain relative paths are allowed", h.Name)
		}
		if _, dup := got[h.Name]; dup || (h.Name == ManifestName && manifest != nil) {
			return nil, refuse("%q is in the archive twice", h.Name)
		}
		if h.Name == ManifestName {
			if manifest, err = io.ReadAll(io.LimitReader(tr, maxManifest+1)); err != nil {
				return nil, unreadable("reading the manifest", err)
			}
			if len(manifest) > maxManifest {
				return nil, refuse("the manifest is over %d MB", maxManifest>>20)
			}
			continue
		}
		if len(got) == l.MaxFiles {
			return nil, refuse("more than %d files", l.MaxFiles)
		}
		left := l.MaxBytes - total
		if h.Size > left {
			return nil, refuse("over %s once uncompressed", size(l.MaxBytes))
		}
		f, err := extract(root, h.Name, tr, left)
		if err != nil {
			return nil, err
		}
		total += f.Size
		got[h.Name] = f
	}
	if manifest == nil {
		return nil, refuse("no %s", ManifestName)
	}
	var m Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, refuse("bad %s: %v", ManifestName, err)
	}
	switch {
	case m.Format > Format:
		return nil, refuse("format %d is newer than this instance reads (%d): update the instance", m.Format, Format)
	case m.Format < 1:
		return nil, refuse("format %d is not a format this instance knows", m.Format)
	}
	for name, f := range got {
		want, ok := m.Files[name]
		switch {
		case !ok:
			return nil, refuse("%s is not in the manifest", name)
		case want.Size != f.Size || want.SHA256 != f.SHA256:
			return nil, refuse("sha256 mismatch on %s: the archive was changed after export", name)
		}
	}
	for name := range m.Files {
		if _, ok := got[name]; !ok {
			return nil, refuse("%s is in the manifest but not in the archive", name)
		}
	}
	return &m, nil
}

// extract writes one entry under root, reading at most left bytes of it, and hashes it.
func extract(root *os.Root, name string, r io.Reader, left int64) (File, error) {
	// A file and a directory of one name: the archive is malformed, not the server.
	if d := path.Dir(name); d != "." {
		if err := root.MkdirAll(d, 0o700); err != nil {
			return File{}, refuse("%s: %v", name, err)
		}
	}
	out, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return File{}, refuse("%s: %v", name, err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(r, left+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return File{}, unreadable("reading "+name, err)
	}
	if n > left {
		return File{}, refuse("over the size limit once uncompressed")
	}
	return File{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// size renders a limit for a refusal.
func size(n int64) string {
	if n < 1<<20 {
		return fmt.Sprintf("%d bytes", n)
	}
	return fmt.Sprintf("%d MB", n>>20)
}

func entryKind(t byte) string {
	switch t {
	case tar.TypeSymlink:
		return "a symlink"
	case tar.TypeLink:
		return "a hard link"
	case tar.TypeChar, tar.TypeBlock:
		return "a device"
	case tar.TypeFifo:
		return "a FIFO"
	case tar.TypeDir:
		return "a directory"
	}
	return fmt.Sprintf("type %q", t)
}
