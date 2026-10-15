package devicestt

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
)

// tempPattern names partial downloads. They live in the model directory itself: in the init
// container it is the only writable path, and a rename within it is atomic.
const tempPattern = ".fetch-*"

// NewClient returns the client Fetch uses in production: the proxy from the environment, bounded
// connection setup, and a whole-download limit generous for 400 MB on a slow link.
func NewClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Minute,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
}

// Fetch puts every file of m into dir. A file already there with its pinned hash is skipped; any
// other is downloaded to a temporary file in dir and renamed once its size and hash check (010-AC6).
// An archive is downloaded only when one of its members is needed, and checked before extraction.
func (m Manifest) Fetch(ctx context.Context, dir string, client *http.Client, log *zap.Logger) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the files are public
		return err
	}
	// Held for the whole run: the sweep below must not take a concurrent run's in-flight file.
	release, err := lockDir(filepath.Join(dir, ".fetch.lock"))
	if err != nil {
		return err
	}
	defer release()
	// Leftovers of a run killed mid-download would otherwise pile up on the volume.
	stale, _ := filepath.Glob(filepath.Join(dir, tempPattern)) // the pattern is valid
	for _, p := range stale {
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	var members []File
	for _, f := range m.Files {
		if f.check(dir) == nil {
			log.Info("model_file_skipped", zap.String("file", f.Name))
			continue
		}
		// A wrong file must not stay under the final name if the download then fails.
		if err := os.Remove(filepath.Join(dir, f.Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		if f.Archive != nil {
			members = append(members, f)
			continue
		}
		if err := download(ctx, client, dir, f.URL, f.Size, f.SHA256, func(tmp string) error {
			return os.Rename(tmp, filepath.Join(dir, f.Name))
		}); err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		log.Info("model_file_downloaded", zap.String("file", f.Name), zap.Int64("bytes", f.Size))
	}
	for len(members) > 0 {
		a := members[0].Archive
		var these, rest []File
		for _, f := range members {
			if f.Archive == a {
				these = append(these, f)
			} else {
				rest = append(rest, f)
			}
		}
		if err := download(ctx, client, dir, a.URL, a.Size, a.SHA256, func(tmp string) error {
			return extract(tmp, dir, these, log)
		}); err != nil {
			return fmt.Errorf("%s: %w", a.URL, err)
		}
		members = rest
	}
	return nil
}

// download writes url to a temporary file in dir, refusing more than size bytes, checks its size
// and sha256, then hands it to done. The temporary file is gone when download returns.
func download(ctx context.Context, client *http.Client, dir, url string, size int64, sum string, done func(tmp string) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // read only
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET: %s", resp.Status)
	}
	tmp, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone already after a rename
	if err := copyChecked(tmp, resp.Body, size, sum); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return done(tmp.Name())
}

// copyChecked copies src to dst, a fresh file, made world-readable: the files are public, and the
// server may read them as another user than the fetch.
func copyChecked(dst *os.File, src io.Reader, size int64, sum string) error {
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(src, size+1))
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("got %d bytes or more, want %d", n, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return fmt.Errorf("sha256 %s, want %s", got, sum)
	}
	return dst.Chmod(0o644)
}

// extract writes the members files name out of the checked archive at path.
func extract(path, dir string, files []File, log *zap.Logger) error {
	f, err := os.Open(path) //nolint:gosec // our own temporary file
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read only
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	want := make(map[string]File, len(files))
	for _, fl := range files {
		want[fl.Member] = fl
	}
	tr := tar.NewReader(gz)
	for len(want) > 0 {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		fl, ok := want[hdr.Name]
		if !ok || hdr.Typeflag != tar.TypeReg {
			continue
		}
		if err := writeMember(tr, dir, fl); err != nil {
			return fmt.Errorf("%s: %w", fl.Name, err)
		}
		delete(want, hdr.Name)
		log.Info("model_file_downloaded", zap.String("file", fl.Name), zap.Int64("bytes", fl.Size))
	}
	for m := range want {
		return fmt.Errorf("%s: not in the archive", m)
	}
	return nil
}

func writeMember(src io.Reader, dir string, f File) error {
	tmp, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone already after a rename
	if err := copyChecked(tmp, src, f.Size, f.SHA256); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, f.Name))
}
