package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"
	"time"

	"go.uber.org/zap"
	"golang.org/x/mod/semver"

	"github.com/babs/grill-room/internal/sandbox"
)

// target names this build in the room server's release, e.g. linux-amd64.
const target = runtime.GOOS + "-" + runtime.GOARCH

// releaseFile names the daemon for goos and goarch in the room server's release.
func releaseFile(goos, goarch string) string {
	if goos == "windows" {
		return "grill-room-daemon-" + goos + "-" + goarch + ".exe"
	}
	return "grill-room-daemon-" + goos + "-" + goarch
}

// maxBinary bounds a downloaded daemon binary.
const maxBinary = 200 << 20

// serverInfo is the room server's GET /api/daemon/version.
type serverInfo struct {
	Version      string            `json:"version"`
	SHA256       map[string]string `json:"sha256"`
	Bundles      map[string]string `json:"bundles"`
	SandboxImage string            `json:"sandbox_image"`
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
		announce = avail != "" && avail != s.Update.Available
		s.Update.Available, s.Update.Served = avail, info.Version
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
