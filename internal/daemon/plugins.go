package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/plugin"
	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/turn"
)

const (
	// maxPluginBytes is the highest snapshot cap a room server may set (plugin_max_mb, 200): the
	// server enforced its own, lower one already.
	maxPluginBytes = 200 << 20
	// maxValidateJSON is what the room keeps of `claude plugin validate --json`.
	maxValidateJSON = 64 << 10
	// pluginCacheTTL drops a cached snapshot no turn loaded for that long.
	pluginCacheTTL = 30 * 24 * time.Hour
)

// inspectTimeout bounds one inspection's container (009-AC13); a var for tests.
var inspectTimeout = 60 * time.Second

// openChecked opens the snapshot at path and returns it rewound, unless plugin.ValidateTar
// refuses it or its sha256 is not sum.
func openChecked(path, sum string) (*os.File, error) {
	f, err := os.Open(path) //nolint:gosec // a fixed name under the instance's own state dir
	if err != nil {
		return nil, err
	}
	s, err := plugin.ValidateTar(f, maxPluginBytes)
	if err == nil && s.SHA256 != sum {
		err = fmt.Errorf("the snapshot's sha256 is %s, not %s", s.SHA256, sum)
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = f.Close() // read only
		return nil, err
	}
	return f, nil
}

// turnPlugins returns the snapshots of the plugins the job lists, from the plugin cache or the
// room (009-AC17), and the function that closes them once the turn has read them.
func (r *roomRun) turnPlugins(ctx context.Context, tok, ids string, ps []turn.Plugin) ([]sandbox.PluginTar, func(), error) {
	out := make([]sandbox.PluginTar, 0, len(ps))
	var open []io.Closer
	closeAll := func() {
		for _, c := range open {
			_ = c.Close() // read only
		}
	}
	for _, p := range ps {
		rc, err := r.pluginTar(ctx, tok, ids, p.SHA256)
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("could not fetch the plugin %s: %w", p.Name, err)
		}
		open = append(open, rc)
		out = append(out, sandbox.PluginTar{Name: p.Name, Tar: rc})
	}
	return out, closeAll, nil
}

// pluginTar returns the snapshot whose sha256 is sum, read from the cache file once checked so a
// turn never holds it in memory. A cached copy is checked on every read: a corrupt one is fetched
// again and replaced.
func (r *roomRun) pluginTar(ctx context.Context, tok, ids, sum string) (io.ReadCloser, error) {
	// sum becomes a file name and a URL path.
	if !sha256Re.MatchString(sum) {
		return nil, fmt.Errorf("%q is not a sha256", sum)
	}
	log := r.d.o.Log.With(zap.String("sha256", sum))
	dir := r.d.o.PluginCacheDir
	path := filepath.Join(dir, sum+".tar")
	if dir != "" {
		f, err := openChecked(path, sum)
		switch {
		case err == nil:
			now := time.Now()
			_ = os.Chtimes(path, now, now) // marks it used for the prune; a failure only shortens its life
			return f, nil
		case !errors.Is(err, fs.ErrNotExist):
			log.Warn("plugin_cache_corrupt", zap.Error(err))
		}
	}
	resp, err := r.get(ctx, tok, "turns/"+ids+"/plugins/"+sum)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	s, err := plugin.ValidateTar(resp.Body, maxPluginBytes)
	if err != nil {
		return nil, err
	}
	if s.SHA256 != sum {
		return nil, fmt.Errorf("the room sent a snapshot whose sha256 is %s, not %s", s.SHA256, sum)
	}
	if dir != "" {
		err := writeFileAtomic(path, s.Tar)
		if err == nil {
			prunePluginCache(dir, time.Now().Add(-pluginCacheTTL), log)
			var f *os.File
			if f, err = os.Open(path); err == nil { //nolint:gosec // see openChecked
				return f, nil
			}
		}
		// The turn runs from memory when the disk refuses: only the next one fetches again.
		log.Warn("plugin_cache_write_failed", zap.Error(err))
	}
	return io.NopCloser(bytes.NewReader(s.Tar)), nil
}

// truncateLine is out's first line, at most 200 bytes, for an error the room shows.
func truncateLine(out []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if len(line) > 200 {
		line = line[:200]
	}
	return line
}

// prunePluginCache removes the cached snapshots last used before cutoff.
func prunePluginCache(dir string, cutoff time.Time, log *zap.Logger) {
	es, err := os.ReadDir(dir)
	if err != nil {
		log.Warn("plugin_cache_prune_failed", zap.Error(err))
		return
	}
	for _, e := range es {
		info, err := e.Info()
		if err != nil || !strings.HasSuffix(e.Name(), ".tar") || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			log.Warn("plugin_cache_prune_failed", zap.Error(err))
		}
	}
}

// pluginReport is the inspection the daemon posts (docs/agent-workspace.md, Plugin inspection).
type pluginReport struct {
	OK       bool            `json:"ok"`
	Validate json.RawMessage `json:"validate,omitempty"`
	Details  *plugin.Details `json:"details,omitempty"`
	Error    string          `json:"error,omitempty"`
	// AllowlistMissing are the domains the plugin's MCP servers name that the egress allowlist
	// does not let the sandbox reach, as it stood at the inspection (009-AC25).
	AllowlistMissing []string `json:"allowlist_missing,omitempty"`
}

// validation is the part of `claude plugin validate --json` the daemon reads (Claude Code 2.1.296).
type validation struct {
	Success  bool `json:"success"`
	Manifest *struct {
		Errors []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"manifest"`
}

// errPluginFailed marks an inspection failure that comes from the plugin itself: claude plugin
// validate printing something other than JSON, or not finishing in time. Any other failure (the
// image, Docker, the room) is the host's or the room's, and never refuses the version.
var errPluginFailed = errors.New("the plugin could not be checked")

const (
	// pluginAttempts is how many times the plugin itself may fail before the room hears it.
	pluginAttempts = 2
	// maxInspectBackoff caps the wait between two tries after a host or room failure.
	maxInspectBackoff = 5 * time.Minute
)

// inspect checks plugin version id in a throwaway container and posts the report (009-AC13). A
// host or room failure is tried again, with a backoff doubling up to maxInspectBackoff, for as long
// as the stream lives, and posts nothing. A failure of the plugin itself, twice, is reported with
// its error, so the room shows why instead of waiting.
func (r *roomRun) inspect(ctx context.Context, tok string, id int64) {
	log := r.d.o.Log.With(zap.String("room", r.room.String()), zap.Int64("version", id))
	wait := r.d.o.MaxBackoff
	pluginFails := 0
	var rep *pluginReport
	for {
		done, err := r.inspectOnce(ctx, tok, id, rep)
		if done || ctx.Err() != nil {
			return
		}
		log.Warn("plugin_inspect_failed", zap.Error(err))
		if rep == nil && errors.Is(err, errPluginFailed) {
			if pluginFails++; pluginFails == pluginAttempts {
				rep = &pluginReport{Error: err.Error()}
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, max(maxInspectBackoff, r.d.o.MaxBackoff))
	}
}

// inspectOnce inspects version id, or posts rep when set, and reports whether the version is done
// with. One inspection runs at a time on this daemon, and a version reported already is skipped:
// the room announces every waiting version again on each new stream.
func (r *roomRun) inspectOnce(ctx context.Context, tok string, id int64, rep *pluginReport) (bool, error) {
	d := r.d
	// Waited for before the lock: an image still building must not hold the other rooms' versions.
	img, err := d.readyImage(ctx)
	if err != nil && rep == nil {
		return false, err
	}
	d.inspectMu.Lock()
	defer d.inspectMu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	key := r.room.String() + "#" + strconv.FormatInt(id, 10)
	if d.inspected[key] {
		return true, nil
	}
	if rep == nil {
		if rep, err = r.runInspection(ctx, tok, id, img); err != nil {
			return false, err
		}
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := d.roomRequest(pctx, http.MethodPost, r.daemonURL("plugins/"+strconv.FormatInt(id, 10)+"/report"), tok, rep)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var ans struct {
		State string `json:"state"`
	}
	switch resp.StatusCode {
	case http.StatusOK:
		_ = json.NewDecoder(resp.Body).Decode(&ans) // the state is only logged
	case http.StatusConflict:
		ans.State = "inspected already"
	default:
		return false, apiError(resp)
	}
	d.inspected[key] = true
	d.o.Log.Info("plugin_inspected", zap.String("room", r.room.String()), zap.Int64("version", id), zap.Bool("ok", rep.OK),
		zap.String("state", ans.State), zap.String("error", rep.Error))
	return true, nil
}

// readyImage waits until the sandbox image is ready, and fails once it failed: a daemon starting
// hears of the waiting versions before its image is checked.
func (d *Daemon) readyImage(ctx context.Context) (Image, error) {
	for {
		changed := d.changes()
		img := d.Status().Image
		switch img.State {
		case "ready":
			return img, nil
		case "failed":
			return img, fmt.Errorf("the sandbox image failed: %s", img.Error)
		}
		d.o.Log.Debug("plugin_inspect_waits_for_image", zap.String("image", img.State))
		select {
		case <-ctx.Done():
			return Image{}, ctx.Err()
		case <-changed:
		}
	}
}

// runInspection fetches version id's snapshot and runs `claude plugin validate --json` and
// `claude plugin details` on it in a container from the sandbox image, with no network, a
// read-only root and a temporary home. Only an invalid tar or a failed validation makes a report
// that refuses the version; any other failure is an error for inspect to retry, errPluginFailed
// when the plugin itself caused it.
func (r *roomRun) runInspection(ctx context.Context, tok string, id int64, img Image) (*pluginReport, error) {
	fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := r.get(fctx, tok, "plugins/"+strconv.FormatInt(id, 10)+"/snapshot")
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	snap, err := plugin.ValidateTar(resp.Body, maxPluginBytes)
	_ = resp.Body.Close()
	if err != nil {
		return &pluginReport{Error: "the snapshot is not a valid plugin: " + err.Error()}, nil
	}
	var name string
	var missing []string
	if m, err := snap.Manifest(); err == nil && plugin.CheckName(m.Name) == nil {
		name = m.Name
		allow := r.d.Status().Allowlist
		for _, dom := range m.Domains {
			if !sandbox.Allowed(allow, dom) {
				missing = append(missing, dom)
			}
		}
	}
	ictx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	run := r.d.o.Docker
	c := "grill-room-inspect-" + r.room.ID + "-" + strconv.FormatInt(id, 10)
	_, _ = run(ictx, nil, "rm", "-f", c) // a crashed daemon's container of the same name
	// Security: the plugin is untrusted until approved. Neither command runs its hooks, MCP servers
	// or bin/ (a Docker test checks the hooks); the container still gets no network, a read-only
	// root, capped resources, and sleep ends it by itself should the removal below never run.
	args := []string{"run", "-d", "--rm", "--name", c, "--label", sandbox.Label + "=" + c}
	for _, l := range r.d.o.Labels {
		args = append(args, "--label", l)
	}
	args = append(args, "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", "1g", "--memory-swap", "1g", "--cpus", "1", "--pids-limit", "64",
		"--tmpfs", "/tmp:size="+strconv.Itoa(2*maxPluginBytes>>20)+"m",
		"-e", "HOME=/tmp", "-e", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"--entrypoint", "sleep", img.Ref, strconv.Itoa(int(inspectTimeout/time.Second)))
	if _, err := run(ictx, nil, args...); err != nil {
		return nil, fmt.Errorf("start the inspection container: %w", err)
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_, _ = run(rctx, nil, "rm", "-f", c) // --rm and sleep's end remove it anyway
		cancel()
	}()
	const dir = "/tmp/p"
	if _, err := run(ictx, nil, "exec", c, "mkdir", dir); err != nil {
		return nil, fmt.Errorf("unpack the snapshot: %w", err)
	}
	if _, err := run(ictx, bytes.NewReader(snap.Tar), "exec", "-i", c, "tar", "-x", "-C", dir); err != nil {
		return nil, fmt.Errorf("unpack the snapshot: %w", err)
	}
	// A failed validation exits 1 with its JSON on stdout.
	out, runErr := run(ictx, nil, "exec", c, "claude", "plugin", "validate", "--json", dir)
	var v validation
	if err := json.Unmarshal(out, &v); err != nil {
		switch {
		case errors.Is(ictx.Err(), context.DeadlineExceeded):
			return nil, fmt.Errorf("%w: claude plugin validate did not finish within %s", errPluginFailed, inspectTimeout)
		case len(bytes.TrimSpace(out)) > 0:
			return nil, fmt.Errorf("%w: claude plugin validate printed no JSON: %s", errPluginFailed, truncateLine(out))
		}
		// Nothing printed: Docker failed, not the plugin.
		return nil, fmt.Errorf("claude plugin validate: %w", errors.Join(runErr, err))
	}
	rep := &pluginReport{OK: v.Success && runErr == nil, AllowlistMissing: missing}
	var compact bytes.Buffer
	if json.Compact(&compact, out) == nil && compact.Len() <= maxValidateJSON {
		rep.Validate = compact.Bytes()
	}
	if !rep.OK {
		rep.Error = "claude plugin validate failed"
		if v.Manifest != nil && len(v.Manifest.Errors) > 0 {
			e := v.Manifest.Errors[0]
			rep.Error = e.Path + ": " + e.Message
		}
		return rep, nil
	}
	if name != "" {
		// Without its details the room shows the cost unknown; the validation stands.
		out, err := run(ictx, nil, "exec", c, "claude", "--plugin-dir", dir, "plugin", "details", name)
		if err != nil {
			r.d.o.Log.Warn("plugin_details_failed", zap.Int64("version", id), zap.Error(err))
		}
		det := plugin.ParseDetails(string(out))
		rep.Details = &det
	}
	return rep, nil
}
