package daemon

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/mod/semver"

	"github.com/babs/grill-room/internal/trust"
)

// minWorkerVersion is the first release a supervisor runs as a worker: v0.0.1 cannot say hello,
// and v0.0.2 reads the Claude credentials itself instead of using the supervisor's proxies (003-AC14).
const minWorkerVersion = "v0.0.3"

// maxBundle bounds a downloaded sigstore bundle.
const maxBundle = 1 << 20

const bundleExt = ".sigstore.json"

// Binaries picks each instance's worker binary: the supervisor's own when the instance serves the
// same version, else the instance's release for this platform, downloaded once per version into
// Dir/<version>/ and verified before every run (003-AC2, 003-AC7).
type Binaries struct {
	// Dir is <state>/bin.
	Dir string
	// Self is the supervisor's own binary, at Version.
	Self    string
	Version string
	HTTP    *http.Client
	Roots   *trust.Roots
	Keys    *DevKeys
	Log     *zap.Logger

	mu sync.Mutex
	// inUse maps an instance to the version it runs; Dir keeps only these versions.
	inUse map[string]string
}

// Resolved is the binary an instance's worker runs.
type Resolved struct {
	Path    string
	Version string
	// Served is the version the instance hands out, "" when it did not answer.
	Served string
	// Trust says why the binary may run, for the page.
	Trust string
	// Relaxed is set when it runs only because the instance accepts unsigned binaries.
	Relaxed bool
	// DevKey names the dev key that signed it, if one did.
	DevKey string
	// Blocked says why the served version does not run: its workers speak a protocol this
	// supervisor does not. Another binary runs meanwhile, and the app must update.
	Blocked string
}

// Init records the versions the instances ran last, then removes every other one and any
// download a crash left (003-AC7).
func (b *Binaries) Init(insts []Instance) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inUse = map[string]string{}
	for _, in := range insts {
		if in.Version != b.Version {
			b.inUse[in.ID] = in.Version
		}
	}
	if tmps, err := filepath.Glob(filepath.Join(b.Dir, ".dl-*")); err == nil {
		for _, f := range tmps {
			_ = os.Remove(f)
		}
	}
	b.gc()
}

// use records that instance runs version, and drops the versions no instance runs any more.
func (b *Binaries) use(instance, version string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inUse == nil {
		b.inUse = map[string]string{}
	}
	if old, ok := b.inUse[instance]; ok && old == version {
		return
	}
	b.inUse[instance] = version
	b.gc()
}

// gc removes the version directories no instance uses; the caller holds mu.
func (b *Binaries) gc() {
	entries, err := os.ReadDir(b.Dir)
	if err != nil {
		return
	}
	keep := map[string]bool{}
	for _, v := range b.inUse {
		keep[v] = true
	}
	for _, e := range entries {
		if !e.IsDir() || keep[e.Name()] || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		// On Windows a running worker's binary cannot be removed: the next collection retries.
		if err := os.RemoveAll(filepath.Join(b.Dir, e.Name())); err != nil {
			b.Log.Warn("binary_gc_failed", zap.String("version", e.Name()), zap.Error(err))
			continue
		}
		b.Log.Info("binary_removed", zap.String("version", e.Name()))
	}
}

// own is the supervisor's binary, for an instance serving its version or none. A stored copy of
// that version, left from before a self-update, is no longer needed.
func (b *Binaries) own(instance, served string) Resolved {
	b.use(instance, "")
	return Resolved{Path: b.Self, Version: b.Version, Served: served, Trust: "the supervisor's own binary"}
}

// Resolve returns the binary in's worker runs. A binary that fails the checks is an errRefused:
// retrying does not help until the instance, the dev keys or the relax toggle change.
func (b *Binaries) Resolve(ctx context.Context, in Instance) (Resolved, error) {
	info, err := fetchServerInfo(ctx, b.HTTP, in.URL, true)
	if err != nil {
		// Offline: the copy verified last time still runs, re-verified from disk.
		b.Log.Warn("instance_version_unknown", zap.String("instance", in.ID), zap.Error(err))
		if in.Version != "" && in.Version != b.Version && in.Version != in.Avoid && semver.Compare(in.Version, minWorkerVersion) >= 0 {
			if r, err := b.stored(in, in.Version, ""); err == nil {
				return r, nil
			}
		}
		return b.own(in.ID, ""), nil
	}
	v := info.Version
	if v == "" || v == b.Version {
		return b.own(in.ID, v), nil
	}
	if lo, hi := ProtoRange(); v == in.Avoid || (info.Proto != 0 && (info.Proto < lo || info.Proto > hi)) {
		return b.keep(in, v, fmt.Sprintf("%s hands out %s, which this app (%s) cannot run", urlHost(in.URL), v, b.Version)), nil
	}
	if !semver.IsValid(v) {
		return Resolved{}, errRefused{fmt.Sprintf("the instance serves version %q, which is not a semver", v)}
	}
	if semver.Compare(v, minWorkerVersion) < 0 {
		return Resolved{}, errRefused{fmt.Sprintf("the instance serves %s, older than %s, the first version that runs as a worker without Claude credentials", v, minWorkerVersion)}
	}
	sum := info.SHA256[target]
	if sum == "" {
		return Resolved{}, errRefused{fmt.Sprintf("the instance serves %s with no binary for %s", v, target)}
	}
	// Recorded before the checks, so that a collection cannot remove the copy being checked.
	b.use(in.ID, v)
	if r, err := b.stored(in, v, sum); err == nil {
		r.Served = v
		return r, nil
	}
	bundleURL := ""
	if p := info.Bundles[target]; strings.HasPrefix(p, "/") {
		bundleURL = in.URL + p
	}
	tmp, r, err := b.obtain(ctx, in.URL+downloadDir(info.Download)+releaseFile(runtime.GOOS, runtime.GOARCH), bundleURL, sum, b.Dir, v, in.Relax)
	if err != nil {
		return Resolved{}, err
	}
	defer os.Remove(tmp.bin) //nolint:errcheck // gone after the rename
	r.Served = v
	b.mu.Lock()
	defer b.mu.Unlock()
	dir := filepath.Join(b.Dir, v)
	r.Path = filepath.Join(dir, releaseFile(runtime.GOOS, runtime.GOARCH))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Resolved{}, err
	}
	if err := os.Rename(tmp.bin, r.Path); err != nil {
		return Resolved{}, err
	}
	// The bundle is kept, so that the copy can be verified again offline.
	if tmp.bundle != nil {
		err = writeFileAtomic(r.Path+bundleExt, tmp.bundle)
	} else {
		err = os.Remove(r.Path + bundleExt)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	}
	if err != nil {
		return Resolved{}, err
	}
	b.Log.Info("binary_stored", zap.String("instance", in.ID), zap.String("version", v), zap.String("trust", r.Trust))
	return r, nil
}

// keep is the binary an instance runs while the version it serves is one this supervisor cannot
// talk to: the copy it ran last, else the supervisor's own. The rooms stay hosted; the page asks for
// an update.
func (b *Binaries) keep(in Instance, served, reason string) Resolved {
	b.Log.Warn("instance_version_incompatible", zap.String("instance", in.ID), zap.String("served", served))
	if v := in.Version; v != "" && v != b.Version && v != served && v != in.Avoid && semver.Compare(v, minWorkerVersion) >= 0 {
		if r, err := b.stored(in, v, ""); err == nil {
			r.Served, r.Blocked = served, reason
			return r
		}
	}
	r := b.own(in.ID, served)
	r.Blocked = reason
	return r
}

// urlHost is a server's host name, as the page names it.
func urlHost(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return u
}

// downloadDir is where the server says its binaries are, "/download/" unless it names another
// directory of its own: a previous release it keeps for older supervisors.
func downloadDir(dir string) string {
	if strings.HasPrefix(dir, "/download/") && strings.HasSuffix(dir, "/") && !strings.Contains(dir, "..") {
		return dir
	}
	return "/download/"
}

// stored verifies the copy of version kept on disk; sum, when set, is the digest the instance
// publishes now.
func (b *Binaries) stored(in Instance, version, sum string) (Resolved, error) {
	path := filepath.Join(b.Dir, version, releaseFile(runtime.GOOS, runtime.GOARCH))
	digest, err := fileSHA256(path)
	if err != nil {
		return Resolved{}, err
	}
	if sum != "" && hex.EncodeToString(digest) != sum {
		return Resolved{}, errors.New("the stored copy differs from the published one")
	}
	bundle, err := os.ReadFile(path + bundleExt) //nolint:gosec // our own state file
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Resolved{}, err
	}
	r, err := b.check(path, digest, bundle, version, in.Relax)
	if err != nil {
		return Resolved{}, err
	}
	b.use(in.ID, version)
	r.Path = path
	return r, nil
}

// download is a binary and its bundle fetched beside their destination.
type download struct {
	bin    string
	bundle []byte
}

// obtain downloads a binary into dir and checks it: its sha256 against wantSHA when set, its
// platform, and its bundle. The caller renames or removes the file.
func (b *Binaries) obtain(ctx context.Context, binURL, bundleURL, wantSHA, dir, version string, relax bool) (download, Resolved, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return download{}, Resolved{}, err
	}
	f, err := os.CreateTemp(dir, ".dl-*")
	if err != nil {
		return download{}, Resolved{}, fmt.Errorf("cannot write in %s: %w", dir, err)
	}
	dl := download{bin: f.Name()}
	fail := func(err error) (download, Resolved, error) {
		_ = f.Close()
		_ = os.Remove(dl.bin)
		return download{}, Resolved{}, err
	}
	h := sha256.New()
	if err := get(ctx, b.HTTP, binURL, maxBinary, io.MultiWriter(f, h)); err != nil {
		return fail(fmt.Errorf("download %s: %w", binURL, err))
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	digest := h.Sum(nil)
	if wantSHA != "" && hex.EncodeToString(digest) != wantSHA {
		return fail(errRefused{fmt.Sprintf("%s: the download does not match the sha256 its server publishes", binURL)})
	}
	if bundleURL != "" {
		var buf strings.Builder
		err := get(ctx, b.HTTP, bundleURL, maxBundle, &buf)
		var nf errNotFound
		switch {
		case errors.As(err, &nf):
		case err != nil:
			return fail(fmt.Errorf("download %s: %w", bundleURL, err))
		default:
			dl.bundle = []byte(buf.String())
		}
	}
	r, err := b.check(dl.bin, digest, dl.bundle, version, relax)
	if err != nil {
		return fail(err)
	}
	// Executable, but only for this user: the supervisor runs it, nobody else needs to.
	if err := os.Chmod(dl.bin, 0o700); err != nil { //nolint:gosec // an executable must be executable
		return fail(err)
	}
	return dl, r, nil
}

// check decides whether the binary at path, with this digest and bundle, may run as version.
func (b *Binaries) check(path string, digest, bundle []byte, version string, relax bool) (Resolved, error) {
	r := Resolved{Version: version}
	reason := "no signature bundle"
	if len(bundle) > 0 {
		tm, _ := b.Roots.Get()
		s, err := trust.Verify(tm, bundle, digest, version, b.Keys.List())
		switch {
		case err != nil:
			reason = "signature refused: " + err.Error()
		case s.Kind == "dev-key":
			reason, r.Trust, r.DevKey = "", "signed with the dev key "+s.Identity, s.Identity
		default:
			reason, r.Trust = "", "signed by the project ("+s.Identity+")"
		}
	}
	if reason != "" && !relax {
		return Resolved{}, errRefused{fmt.Sprintf("%s refused: %s", version, reason)}
	}
	// Even a relaxed instance cannot hand out a binary that does not run here.
	if err := checkPlatform(path, runtime.GOOS, runtime.GOARCH); err != nil {
		return Resolved{}, errRefused{fmt.Sprintf("%s refused: %v", version, err)}
	}
	if reason != "" {
		r.Trust, r.Relaxed = "unsigned, accepted because this instance accepts unsigned binaries ("+reason+")", true
	}
	return r, nil
}

// checkPlatform refuses a binary built for another OS or architecture: a signature covers the
// bytes, not the platform they were published for.
func checkPlatform(path, goos, goarch string) error {
	f, err := os.Open(path) //nolint:gosec // a file we just downloaded or stored
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only
	want := goos + "/" + goarch
	var got string
	switch goos {
	case "linux":
		ef, err := elf.NewFile(f)
		if err != nil {
			return fmt.Errorf("not a Linux executable, want %s", want)
		}
		got = "linux/" + map[elf.Machine]string{elf.EM_X86_64: "amd64", elf.EM_AARCH64: "arm64"}[ef.Machine]
	case "darwin":
		mf, err := macho.NewFile(f)
		if err != nil {
			return fmt.Errorf("not a macOS executable, want %s", want)
		}
		got = "darwin/" + map[macho.Cpu]string{macho.CpuAmd64: "amd64", macho.CpuArm64: "arm64"}[mf.Cpu]
	case "windows":
		pf, err := pe.NewFile(f)
		if err != nil {
			return fmt.Errorf("not a Windows executable, want %s", want)
		}
		got = "windows/" + map[uint16]string{pe.IMAGE_FILE_MACHINE_AMD64: "amd64", pe.IMAGE_FILE_MACHINE_ARM64: "arm64"}[pf.Machine]
	}
	if got != want {
		return fmt.Errorf("built for %q, want %s", got, want)
	}
	return nil
}

func fileSHA256(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // a binary we stored or run
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

type errNotFound struct{ url string }

func (e errNotFound) Error() string { return e.url + ": 404 Not Found" }

// get copies url's body into w, refusing more than limit bytes.
func get(ctx context.Context, hc *http.Client, url string, limit int64, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound{url}
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err == nil && n > limit {
		err = fmt.Errorf("%s: larger than %d bytes", url, limit)
	}
	return err
}

// fetchServerInfo asks a room server what daemon release it hands out: for a worker, one whose
// protocol is in ProtoRange when the server keeps one; for the supervisor itself, the latest.
func fetchServerInfo(ctx context.Context, hc *http.Client, base string, worker bool) (serverInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var buf strings.Builder
	u := base + "/api/daemon/version"
	if lo, hi := ProtoRange(); worker {
		u += fmt.Sprintf("?proto=%d-%d", lo, hi)
	}
	if err := get(ctx, hc, u, 64<<10, &buf); err != nil {
		return serverInfo{}, err
	}
	var info serverInfo
	if err := json.Unmarshal([]byte(buf.String()), &info); err != nil {
		return serverInfo{}, fmt.Errorf("%s/api/daemon/version: %w", base, err)
	}
	return info, nil
}
