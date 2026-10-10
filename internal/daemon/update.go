package daemon

// The self-update is ported from github.com/babs/claude-quota selfupdate.go and restart_unix.go at
// commit 45b0113, with the paired room server as the source instead of GitHub releases, a sha256
// check against the server's published digest, and an approval in the local page.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"go.uber.org/zap"
	"golang.org/x/mod/semver"

	"github.com/babs/grill-room/internal/sandbox"
)

// target names this build in the room server's release, e.g. linux-amd64.
const target = runtime.GOOS + "-" + runtime.GOARCH

// maxBinary bounds a downloaded daemon binary.
const maxBinary = 200 << 20

// serverInfo is the room server's GET /api/daemon/version.
type serverInfo struct {
	Version      string            `json:"version"`
	SHA256       map[string]string `json:"sha256"`
	SandboxImage string            `json:"sandbox_image"`
}

// release is the room server's daemon build for this platform.
type release struct {
	base, version, sha256 string
}

// imageJob tracks the sandbox image the room server asked for.
type imageJob struct {
	// known is set once a room server answered, or when Options.Image pins the image.
	known bool
	want  string
	// ctx bounds the pull or build: the room run that asked for it.
	ctx context.Context
	// gen numbers jobs: only the latest writes the status, a cancelled one may outlive it.
	gen int
}

// checkServer asks the room server for its daemon release and sandbox image. A server too old to
// answer gets the embedded sandbox image; an unreachable one is asked again on the next connect.
func (r *roomRun) checkServer(ctx context.Context) {
	d := r.d
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := d.roomRequest(rctx, http.MethodGet, r.room.Base+"/api/daemon/version", "", nil)
	if err != nil {
		if ctx.Err() == nil {
			d.o.Log.Warn("server_version_failed", zap.Error(err))
		}
		return
	}
	defer resp.Body.Close()
	var info serverInfo
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&info); err != nil {
			d.o.Log.Warn("server_version_failed", zap.Error(err))
			return
		}
	}
	d.useServer(ctx, r.room.Base, info)
}

// useServer records what the room server at base hands out and starts preparing its sandbox image.
func (d *Daemon) useServer(ctx context.Context, base string, info serverInfo) {
	sum := info.SHA256[target]
	avail := ""
	if semver.IsValid(info.Version) && semver.Compare(info.Version, d.o.Version) > 0 && sum != "" {
		avail = info.Version
	}
	var announce bool
	d.update(func(s *Status) {
		d.release = release{base: base, version: info.Version, sha256: sum}
		announce = avail != "" && avail != s.Update.Available
		s.Update.Available = avail
		if d.o.Image == "" {
			d.img.ctx = ctx
			if !d.img.known || d.img.want != info.SandboxImage {
				d.img.known, d.img.want = true, info.SandboxImage
				d.img.gen++
				s.Image = Image{State: "pending"}
			}
		}
	})
	if announce {
		d.o.Log.Info("update_available", zap.String("version", avail), zap.String("current", d.o.Version))
	}
	d.maybePrepare()
}

// maybePrepare starts the pending image job once Docker runs.
func (d *Daemon) maybePrepare() {
	d.mu.Lock()
	j := d.img
	if !j.known || d.st.Image.State != "pending" || !d.st.Docker.OK || j.ctx == nil || j.ctx.Err() != nil {
		d.mu.Unlock()
		return
	}
	d.img.gen++
	gen := d.img.gen
	d.st.Image = Image{State: "checking"}
	d.mu.Unlock()
	d.loops.Go(func() { d.prepareImage(j.ctx, j.want, gen) })
}

// prepareImage pulls or builds the sandbox image, writing its progress into the status while gen
// is the latest job.
func (d *Daemon) prepareImage(ctx context.Context, want string, gen int) {
	current := func() bool { return d.img.gen == gen }
	ref, err := sandbox.EnsureImage(ctx, d.o.Docker, d.o.Stream, want, func(state, line string) {
		if len(line) > 300 {
			line = line[:300] + "..."
		}
		d.mu.Lock()
		if current() {
			d.st.Image.State, d.st.Image.Progress = state, line
		}
		d.mu.Unlock()
	})
	stale := false
	d.update(func(s *Status) {
		if stale = !current(); stale {
			return
		}
		switch {
		case ctx.Err() != nil:
			// The room run ended mid-job: the next one starts it again.
			s.Image = Image{State: "pending"}
		case err != nil:
			s.Image = Image{State: "failed", Error: err.Error()}
		default:
			s.Image = Image{State: "ready", Ref: ref}
		}
	})
	switch {
	case stale:
	case ctx.Err() != nil:
		// A new room run may already be waiting on this image.
		d.maybePrepare()
	case err != nil:
		d.o.Log.Warn("sandbox_image_failed", zap.String("want", want), zap.Error(err))
	default:
		d.o.Log.Info("sandbox_image_ready", zap.String("image", ref))
	}
}

// Recheck re-runs the readiness checks and retries a failed sandbox image.
func (d *Daemon) Recheck(ctx context.Context) {
	d.mu.Lock()
	if d.st.Image.State == "failed" {
		d.st.Image = Image{State: "pending"}
	}
	d.mu.Unlock()
	d.Refresh(ctx)
}

var errNoUpdate = errors.New("no update available")

// Update replaces this daemon's binary with the room server's newer one, after checking its
// sha256 against the digest the server publishes. The caller restarts the daemon on success.
func (d *Daemon) Update(ctx context.Context) error {
	d.mu.Lock()
	rel, u := d.release, d.st.Update
	if u.Available == "" || u.Available != rel.version {
		d.mu.Unlock()
		return errNoUpdate
	}
	if u.Running {
		d.mu.Unlock()
		return errors.New("an update is already running")
	}
	d.st.Update.Running, d.st.Update.Error = true, ""
	d.mu.Unlock()

	d.o.Log.Info("update_started", zap.String("version", rel.version))
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	err := d.replaceExecutable(ctx, rel.base+"/download/grill-room-daemon-"+target, rel.sha256)
	d.mu.Lock()
	d.st.Update.Running = false
	if err != nil {
		d.st.Update.Error = err.Error()
	}
	d.mu.Unlock()
	if err != nil {
		d.o.Log.Warn("update_failed", zap.Error(err))
		return err
	}
	d.o.Log.Info("update_applied", zap.String("version", rel.version))
	return nil
}

// replaceExecutable downloads url beside the executable, checks its sha256 and renames it over
// the executable. A failure at any step leaves the current binary in place.
func (d *Daemon) replaceExecutable(ctx context.Context, url, wantSHA string) error {
	exe := d.o.Executable
	if exe == "" {
		return errors.New("this daemon cannot locate its own binary")
	}
	// Same directory, so the final rename is atomic.
	tmp, err := os.CreateTemp(filepath.Dir(exe), "."+filepath.Base(exe)+".update-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s (download the new version from %s): %w", exe, url, err)
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	resp, err := d.roomRequest(ctx, http.MethodGet, url, "", nil)
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = tmp.Close()
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxBinary))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		return fmt.Errorf("sha256 mismatch: the room server publishes %s, the download is %s; the current binary is kept", wantSHA, got)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil { //nolint:gosec // an executable must be executable
		return fmt.Errorf("chmod: %w", err)
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		return fmt.Errorf("replace %s: %w", exe, err)
	}
	return nil
}
