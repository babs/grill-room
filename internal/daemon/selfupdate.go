package daemon

// The self-update is ported from github.com/babs/claude-quota selfupdate.go and restart_unix.go at
// commit 45b0113: GitHub releases or an instance as the source, a sigstore bundle checked before
// the binary replaces the running one, and an approval in the local page.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"go.uber.org/zap"
	"golang.org/x/mod/semver"

	"github.com/babs/grill-room/internal/trust"
)

// SupervisorSettings are the supervisor's own settings, in supervisor.json.
type SupervisorSettings struct {
	// UpdateChannel is "github" (the default) or "instance".
	UpdateChannel string `json:"update_channel"`
	// UpdateInstance is the instance the "instance" channel follows.
	UpdateInstance string `json:"update_instance,omitempty"`
	// MaxTurns is the global cap on concurrent turns, DefaultMaxTurns when 0 (003-AC18).
	MaxTurns int `json:"max_turns,omitempty"`
}

// maxTurns is the cap in force.
func (s SupervisorSettings) maxTurns() int {
	if s.MaxTurns == 0 {
		return DefaultMaxTurns
	}
	return s.MaxTurns
}

func loadSupervisorSettings(path string) (SupervisorSettings, error) {
	set := SupervisorSettings{UpdateChannel: "github"}
	b, err := os.ReadFile(path) //nolint:gosec // our own config file
	if errors.Is(err, os.ErrNotExist) {
		return set, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &set)
	}
	if err != nil || (set.UpdateChannel != "github" && set.UpdateChannel != "instance") || set.MaxTurns < 0 || set.MaxTurns > MaxTurnsLimit {
		return SupervisorSettings{UpdateChannel: "github"}, fmt.Errorf("%s: %w", path, errors.Join(err, errors.New("falling back to the GitHub channel")))
	}
	return set, nil
}

// selfUpdate is the update on offer for the supervisor's own binary.
type selfUpdate struct {
	available, from   string
	binURL, bundleURL string
	sha               string
	relax             bool
	running           bool
	err               string
	checkErr          string
	checked           time.Time
}

func (u selfUpdate) view() Update {
	return Update{Available: u.available, From: u.from, Running: u.running, Error: u.err}
}

// SelfSignature says whether the running supervisor binary is project-signed (003-AC5).
type SelfSignature struct {
	// State is "project", "dev-key", "dev build", "unsigned" or "unknown".
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// TrustView is what the page shows of the supervisor's trust and update settings.
type TrustView struct {
	Self    SelfSignature  `json:"self"`
	Root    trust.RootInfo `json:"root"`
	DevKeys []trust.DevKey `json:"dev_keys"`
	Channel string         `json:"channel"`
	// Follow is the instance the "instance" channel follows.
	Follow     string    `json:"follow,omitempty"`
	CheckError string    `json:"check_error,omitempty"`
	Checked    time.Time `json:"checked"`
}

var errNoBins = errors.New("binary verification is not available in this supervisor")

// Trust returns the page's view of the trust settings.
func (s *Supervisor) Trust() TrustView {
	var v TrustView
	if b := s.o.Bins; b != nil {
		v.Root = b.Roots.Info()
		v.DevKeys = b.Keys.List()
	}
	if v.DevKeys == nil {
		v.DevKeys = []trust.DevKey{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v.Self, v.Channel, v.Follow, v.CheckError, v.Checked = s.self, s.set.UpdateChannel, s.set.UpdateInstance, s.upd.checkErr, s.upd.checked
	return v
}

// updateLoop checks whether the supervisor is project-signed, then checks the update channel now,
// every UpdateEvery and whenever the channel changes.
func (s *Supervisor) updateLoop(ctx context.Context) {
	self := s.checkSelf(ctx)
	s.mu.Lock()
	s.self = self
	s.mu.Unlock()
	s.o.Log.Info("self_signature", zap.String("state", self.State), zap.String("detail", self.Detail))
	for {
		s.checkUpdate(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.o.UpdateEvery):
		case <-s.checkNow:
		}
	}
}

// isRelease is a version the release workflow may have signed.
func isRelease(v string) bool {
	return semver.IsValid(v) && semver.Prerelease(v) == "" && semver.Build(v) == "" && v != "v0.0.0"
}

// checkSelf verifies the supervisor's own binary against its bundle: one beside the binary, else
// the release's on GitHub.
func (s *Supervisor) checkSelf(ctx context.Context) SelfSignature {
	exe, b := s.o.Executable, s.o.Bins
	digest, err := fileSHA256(exe)
	if err != nil {
		return SelfSignature{State: "unknown", Detail: err.Error()}
	}
	bundle, err := os.ReadFile(exe + bundleExt) //nolint:gosec // beside our own binary
	if errors.Is(err, os.ErrNotExist) {
		if !isRelease(s.o.Version) {
			return SelfSignature{State: "dev build", Detail: s.o.Version + " is not a release"}
		}
		var buf strings.Builder
		gctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = get(gctx, b.HTTP, s.o.GitHub+"/releases/download/"+s.o.Version+"/"+releaseFile(runtime.GOOS, runtime.GOARCH)+bundleExt, maxBundle, &buf)
		cancel()
		var nf errNotFound
		if errors.As(err, &nf) {
			return SelfSignature{State: "dev build", Detail: "no " + s.o.Version + " release on GitHub"}
		}
		bundle = []byte(buf.String())
	}
	if err != nil {
		return SelfSignature{State: "unknown", Detail: "no bundle to check: " + err.Error()}
	}
	tm, _ := b.Roots.Get()
	sig, err := trust.Verify(tm, bundle, digest, s.o.Version, b.Keys.List())
	if err != nil {
		return SelfSignature{State: "unsigned", Detail: err.Error()}
	}
	return SelfSignature{State: sig.Kind, Detail: sig.Identity}
}

// checkUpdate looks for a version to update to on the chosen channel: a newer GitHub release, or
// the version the followed instance serves.
func (s *Supervisor) checkUpdate(ctx context.Context) {
	s.mu.Lock()
	set := s.set
	var in Instance
	if w := s.workers[set.UpdateInstance]; w != nil {
		in = w.inst
	}
	s.mu.Unlock()
	file := releaseFile(runtime.GOOS, runtime.GOARCH)
	u := selfUpdate{checked: time.Now()}
	var err error
	switch {
	case set.UpdateChannel == "instance" && in.ID == "":
		err = errors.New("the followed instance is gone: pick one again")
	case set.UpdateChannel == "instance":
		var info serverInfo
		if info, err = fetchServerInfo(ctx, s.o.Bins.HTTP, in.URL); err == nil {
			v, sum := info.Version, info.SHA256[target]
			if semver.IsValid(v) && v != s.o.Version && sum != "" {
				u.available, u.from, u.binURL, u.sha, u.relax = v, in.URL, in.URL+"/download/"+file, sum, in.Relax
				if p := info.Bundles[target]; strings.HasPrefix(p, "/") {
					u.bundleURL = in.URL + p
				}
			}
		}
	default:
		var buf strings.Builder
		gctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = get(gctx, s.o.Bins.HTTP, s.o.GitHub+"/releases/latest/download/VERSION", 64, &buf)
		cancel()
		if v := strings.TrimSpace(buf.String()); err == nil && semver.IsValid(v) && semver.Compare(v, s.o.Version) > 0 {
			rel := s.o.GitHub + "/releases/download/" + v + "/" + file
			u.available, u.from, u.binURL, u.bundleURL = v, "GitHub", rel, rel+bundleExt
		}
	}
	if err != nil {
		u.checkErr = err.Error()
		if ctx.Err() == nil {
			s.o.Log.Warn("update_check_failed", zap.String("channel", set.UpdateChannel), zap.Error(err))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upd.running {
		return
	}
	if u.available != "" && u.available != s.upd.available {
		s.o.Log.Info("update_available", zap.String("version", u.available), zap.String("from", u.from), zap.String("current", s.o.Version))
	}
	s.upd = u
}

// Update replaces the supervisor's binary with the version on offer, once its bundle verifies
// (003-AC5). The caller restarts the supervisor on success.
func (s *Supervisor) Update(ctx context.Context) error {
	if s.o.Bins == nil {
		return errNoBins
	}
	s.mu.Lock()
	u := s.upd
	if u.available == "" {
		s.mu.Unlock()
		return errNoUpdate
	}
	if u.running {
		s.mu.Unlock()
		return errors.New("an update is already running")
	}
	s.upd.running, s.upd.err = true, ""
	s.mu.Unlock()
	s.o.Log.Info("update_started", zap.String("version", u.available), zap.String("from", u.from))
	err := s.applyUpdate(ctx, u)
	s.mu.Lock()
	s.upd.running = false
	if err != nil {
		s.upd.err = err.Error()
	}
	s.mu.Unlock()
	if err != nil {
		s.o.Log.Warn("update_failed", zap.Error(err))
	}
	return err
}

func (s *Supervisor) applyUpdate(ctx context.Context, u selfUpdate) error {
	exe := s.o.Executable
	if exe == "" {
		return errors.New("this supervisor cannot locate its own binary")
	}
	dl, r, err := s.o.Bins.obtain(ctx, u.binURL, u.bundleURL, u.sha, filepath.Dir(exe), u.available, u.relax)
	if err != nil {
		return err
	}
	defer os.Remove(dl.bin)                         //nolint:errcheck // gone after the rename
	if err := os.Chmod(dl.bin, 0o755); err != nil { //nolint:gosec // an executable others may run, as before
		return fmt.Errorf("chmod: %w", err)
	}
	if err := os.Rename(dl.bin, exe); err != nil {
		return fmt.Errorf("replace %s: %w", exe, err)
	}
	// Kept beside the binary, so that the next start checks its own signature offline.
	if dl.bundle != nil {
		err = writeFileAtomic(exe+bundleExt, dl.bundle)
	} else if err = os.Remove(exe + bundleExt); errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		s.o.Log.Warn("self_bundle_not_saved", zap.Error(err))
	}
	s.o.Log.Info("update_applied", zap.String("version", u.available), zap.String("trust", r.Trust))
	return nil
}

// SetUpdateChannel picks where the supervisor updates from: "github", or "instance" with the
// instance to follow (the current one when empty).
func (s *Supervisor) SetUpdateChannel(channel, instance string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upd.running {
		return errors.New("an update is running: change the channel once it is done")
	}
	set := s.set
	set.UpdateChannel, set.UpdateInstance = channel, ""
	switch channel {
	case "github":
	case "instance":
		if instance == "" {
			instance = s.current
		}
		if s.workers[instance] == nil {
			return errors.New("no such instance: pair with a room on it first")
		}
		set.UpdateInstance = instance
	default:
		return fmt.Errorf("unknown update channel %q: want github or instance", channel)
	}
	if err := s.saveSettingsLocked(set); err != nil {
		return err
	}
	s.upd = selfUpdate{}
	s.o.Log.Info("update_channel_changed", zap.String("channel", channel), zap.String("instance", instance))
	select {
	case s.checkNow <- struct{}{}:
	default:
	}
	return nil
}

// saveSettingsLocked writes set to supervisor.json and applies it; the caller holds mu.
func (s *Supervisor) saveSettingsLocked(set SupervisorSettings) error {
	b, _ := json.MarshalIndent(set, "", "  ") // plain strings and a number
	if err := writeFileAtomic(s.o.Paths.supervisorSettings(), append(b, '\n')); err != nil {
		return err
	}
	s.set = set
	return nil
}

// SetMaxTurns sets the global cap on concurrent turns across every instance and room (003-AC18).
func (s *Supervisor) SetMaxTurns(n int) error {
	if n < 1 || n > MaxTurnsLimit {
		return fmt.Errorf("the cap on concurrent turns must be 1 to %d", MaxTurnsLimit)
	}
	s.mu.Lock()
	set := s.set
	set.MaxTurns = n
	err := s.saveSettingsLocked(set)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.gate.setMax(n)
	s.o.Log.Info("max_turns_changed", zap.Int("max_turns", n))
	return nil
}

// Concurrency is the global cap, the turns holding or waiting for a slot, and the queue waits.
func (s *Supervisor) Concurrency() ConcurrencyView { return s.gate.view() }

// SetRelax switches an instance's "accept unsigned" toggle (003-AC4). Switching it on needs the
// instance's host typed exactly. The worker restarts under the new rule.
func (s *Supervisor) SetRelax(id string, on bool, host string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.workers[id]
	if w == nil {
		return errors.New("no such instance")
	}
	if on {
		u, err := url.Parse(w.inst.URL)
		if err != nil || host != u.Host {
			return errors.New("type the instance's host exactly, as in its URL, to accept its unsigned binaries")
		}
	}
	if w.inst.Relax == on {
		return nil
	}
	w.inst.Relax = on
	s.save(w)
	if on {
		s.o.Log.Warn("accept_unsigned_on", zap.String("instance", id), zap.String("url", w.inst.URL))
	} else {
		s.o.Log.Info("accept_unsigned_off", zap.String("instance", id))
	}
	s.reload(w)
	return nil
}

// AddDevKey trusts a dev key; refused workers try again with it (003-AC4).
func (s *Supervisor) AddDevKey(ctx context.Context, name string, pemBytes []byte) error {
	if s.o.Bins == nil {
		return errNoBins
	}
	if err := s.o.Bins.Keys.Add(name, pemBytes); err != nil {
		return err
	}
	s.o.Log.Info("dev_key_added", zap.String("name", name))
	s.afterKeyChange(ctx, func(w *worker) bool { return w.state == "refused" })
	return nil
}

// RemoveDevKey stops trusting a dev key; workers running a binary it signed restart.
func (s *Supervisor) RemoveDevKey(ctx context.Context, name string) error {
	if s.o.Bins == nil {
		return errNoBins
	}
	if err := s.o.Bins.Keys.Remove(name); err != nil {
		return err
	}
	s.o.Log.Info("dev_key_removed", zap.String("name", name))
	s.afterKeyChange(ctx, func(w *worker) bool { return w.devKey == name })
	return nil
}

func (s *Supervisor) afterKeyChange(ctx context.Context, affected func(*worker) bool) {
	s.mu.Lock()
	for _, w := range s.workers {
		if affected(w) {
			s.reload(w)
		}
	}
	s.mu.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	// The indicator may change with the keys.
	s.wg.Go(func() {
		self := s.checkSelf(ctx)
		s.mu.Lock()
		s.self = self
		s.mu.Unlock()
	})
}

// reload restarts w so that it resolves its binary again; the caller holds mu. A refused worker
// starts again, a running one is asked to stop, and the run loop restarts it at once.
func (s *Supervisor) reload(w *worker) {
	if s.ctx.Err() != nil {
		// Stopping: the workers are on their way out.
		return
	}
	w.reload = true
	switch w.state {
	case "refused":
		w.reload = false
		w.state, w.err = "starting", ""
		s.wg.Go(w.run)
	case "running":
		if c := w.conn; c != nil {
			go func() { _ = c.Send(Msg{Type: "stop"}) }()
		}
	}
}

// save writes w's instance.json; the caller holds mu, so that writes land in order.
func (s *Supervisor) save(w *worker) { s.saveInstance(w.inst) }

func (s *Supervisor) saveInstance(in Instance) {
	if err := saveInstance(s.o.Paths, in); err != nil {
		s.o.Log.Error("instance_save_failed", zap.String("instance", in.ID), zap.Error(err))
	}
}

// maxLogLine bounds a worker's log entry on the terminal and the page.
const maxLogLine = 16 << 10

// safeLogLine re-encodes a worker's log entry as one JSON object: control characters, C1 codes
// and bidi or zero-width format characters are escaped, so a binary an instance served cannot
// drive the terminal or disguise a line. The instance field is the supervisor's, not the worker's.
func safeLogLine(instance string, entry any) []byte {
	var m map[string]any
	switch e := entry.(type) {
	case json.RawMessage:
		d := json.NewDecoder(bytes.NewReader(e))
		d.UseNumber()
		if d.Decode(&m) != nil || m == nil {
			m = map[string]any{"msg": string(e)}
		}
	case map[string]any:
		m = e
	}
	m["instance"] = instance
	b, err := json.Marshal(m)
	if err != nil || len(b) > maxLogLine {
		b, _ = json.Marshal(map[string]any{"msg": "worker_log_dropped", "instance": instance, "bytes": len(b)})
	}
	// json.Marshal escapes C0 controls and invalid UTF-8; the rest is done here.
	out := make([]byte, 0, len(b))
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r >= 0x80 && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			if r1, r2 := utf16.EncodeRune(r); r1 != unicode.ReplacementChar {
				out = fmt.Appendf(out, `\u%04x\u%04x`, r1, r2)
			} else {
				out = fmt.Appendf(out, `\u%04x`, r)
			}
		} else {
			out = append(out, b[:n]...)
		}
		b = b[n:]
	}
	return out
}

// lineWriter hands each line written to it to emit, without its newline; a line longer than
// maxLogLine is cut.
type lineWriter struct {
	buf  []byte
	emit func([]byte)
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			if len(l.buf) > maxLogLine {
				l.emit(l.buf[:maxLogLine])
				l.buf = l.buf[:0]
			}
			return len(p), nil
		}
		l.emit(l.buf[:i])
		l.buf = l.buf[i+1:]
	}
}
