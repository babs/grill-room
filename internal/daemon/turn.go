package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/turn"
)

// Agent runs turns in a sandbox; *sandbox.Sandbox is the real one.
type Agent interface {
	// Start brings the sandbox up; RunTurn starts it too when it is down.
	Start(ctx context.Context) error
	RunTurn(ctx context.Context, in sandbox.TurnInput) (*sandbox.TurnOutput, error)
	// SetPolicy sets what the egress proxy lets through; a running sandbox takes it at once.
	SetPolicy(ctx context.Context, p sandbox.Policy) error
	// Egress returns the last n refused egress requests, newest first, and the requests let through by host.
	Egress(ctx context.Context, n int) (sandbox.Egress, error)
	// Requests returns the egress requests logged since since, oldest first.
	Requests(ctx context.Context, since time.Time) ([]sandbox.EgressRequest, error)
	Close(ctx context.Context)
}

// maxSession bounds a session copy fetched from the room.
const maxSession = 256 << 20

// known keeps l's fields only when claude knows them: the values become claude arguments, whatever
// the room sent.
func known(l turn.Level) turn.Level {
	if !slices.Contains(turn.Models, l.Model) {
		l.Model = ""
	}
	if !slices.Contains(turn.Efforts, l.Effort) {
		l.Effort = ""
	}
	return l
}

// knownAutocompact keeps n only within the range claude accepts, as it becomes a claude argument
// whatever the room sent; 0 leaves Claude Code on auto (003-AC24).
func knownAutocompact(n int) int {
	if n < turn.MinAutocompact || n > turn.MaxAutocompact {
		return 0
	}
	return n
}

// runSettings is the one place where the model and effort a persona asks for become the ones its
// turn runs with. The host cap, raised by what the host approved for the persona, clamps them
// (002-AC19, 002-AC20). Personas always ask for both (005-AC9): only a room server older than that
// sends neither, and Claude Code picks.
func (d *Daemon) runSettings(job turn.Job) (run turn.Level, capped bool) {
	return turn.Clamp(known(turn.Level{Model: job.Model, Effort: job.Effort}), d.Status().Settings.Limit(), known(job.Approved))
}

// agent takes a free sandbox of the room for one turn, and returns it with the function that frees
// it again. The room's first sandbox serves its turns one after the other; a parallel turn finds
// it busy and gets one of its own (003-AC20). A free sandbox built for another account's proxy is
// rebuilt.
func (r *roomRun) agent(proxy string) (Agent, func(), error) {
	r.agentMu.Lock()
	defer r.agentMu.Unlock()
	i := slices.IndexFunc(r.sandboxes, func(sl *sandboxSlot) bool { return !sl.busy })
	if i < 0 {
		r.sandboxes = append(r.sandboxes, &sandboxSlot{})
		i = len(r.sandboxes) - 1
	}
	sl := r.sandboxes[i]
	if sl.ag != nil && sl.proxy != proxy {
		sl.ag.Close(r.ctx)
		sl.ag = nil
	}
	if sl.ag == nil {
		img := r.d.Status().Image
		if img.State != "ready" {
			return nil, nil, errors.New("the sandbox image is not ready on the host's machine")
		}
		ag := r.d.o.NewAgent(r.room, i, img.Ref, proxy)
		// Under agentMu, read after any saved policy: setEgress hands later ones to every sandbox.
		if err := ag.SetPolicy(r.ctx, r.d.policy(r.room.String())); err != nil {
			return nil, nil, fmt.Errorf("egress allowlist: %w", err)
		}
		sl.ag, sl.proxy = ag, proxy
	}
	sl.busy = true
	return sl.ag, func() {
		r.agentMu.Lock()
		sl.busy = false
		r.agentMu.Unlock()
	}, nil
}

// closeAgent removes the room's sandboxes when the room run ends.
func (r *roomRun) closeAgent(ctx context.Context) {
	r.agentMu.Lock()
	defer r.agentMu.Unlock()
	for _, sl := range r.sandboxes {
		if sl.ag != nil {
			sl.ag.Close(ctx)
		}
	}
	r.sandboxes = nil
}

func (r *roomRun) daemonURL(path string) string {
	return r.room.Base + "/api/rooms/" + r.room.ID + "/daemon/" + path
}

// get fetches path with the daemon token; the caller closes the body of a 200.
func (r *roomRun) get(ctx context.Context, tok, path string) (*http.Response, error) {
	resp, err := r.d.roomRequest(ctx, http.MethodGet, r.daemonURL(path), tok, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, apiError(resp)
	}
	return resp, nil
}

// runTurn runs turn id the room handed this daemon and posts its result. Errors before the run
// only log: the room fails the turn when this daemon's stream drops or its timeout passes.
func (r *roomRun) runTurn(ctx context.Context, tok string, id int64) {
	log := r.d.o.Log.With(zap.String("room", r.room.String()), zap.Int64("turn", id))
	start, ended := time.Now(), "the turn was abandoned"
	var queued time.Duration
	r.d.turnEvent(TurnEvent{Room: r.room.String(), ID: id, Phase: "start"})
	var job turn.Job
	var res turn.Result
	// ev gathers what execute learns of the run; the rest is set here at the end.
	var ev TurnEvent
	defer func() {
		ev.Room, ev.ID, ev.Phase, ev.Error, ev.DurationMS, ev.QueueMS = r.room.String(), id, "end", ended, time.Since(start).Milliseconds(), queued.Milliseconds()
		ev.Persona, ev.ContextTokens, ev.ContextWindow = job.Persona, res.ContextTokens, res.ContextWindow
		ev.Cause, ev.AskedModel, ev.AskedEffort = job.Cause, job.Model, job.Effort
		r.d.turnEvent(ev)
	}()
	ids := strconv.FormatInt(id, 10)
	// Cancellable from the start: a cut-off ends a turn still waiting for a slot, and a stop may
	// come while the job is fetched.
	cctx, cut := context.WithCancelCause(ctx)
	defer cut(nil)
	r.d.mu.Lock()
	r.d.running[id] = cut
	r.d.mu.Unlock()
	defer func() {
		r.d.mu.Lock()
		delete(r.d.running, id)
		r.d.mu.Unlock()
	}()
	jctx, cancel := context.WithTimeout(cctx, 30*time.Second)
	resp, err := r.get(jctx, tok, "turns/"+ids)
	if err == nil {
		err = json.NewDecoder(resp.Body).Decode(&job)
		_ = resp.Body.Close()
	}
	cancel()
	if err != nil {
		log.Warn("turn_job_failed", zap.Error(err))
		// Told now, the room frees the button instead of waiting for its timeout.
		res.Error = "could not fetch the turn: " + err.Error()
		if errors.Is(context.Cause(cctx), errCutOff) {
			res.Error = errCutOff.Error()
		}
	} else {
		timeout := time.Duration(job.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 15 * time.Minute
		}
		release, waited, err := r.slot(cctx, tok, id)
		queued = waited
		switch {
		case errors.Is(context.Cause(cctx), errCutOff):
			res.Error = errCutOff.Error()
		case err != nil:
			res.Error = "could not get a slot on the host's machine: " + err.Error()
		default:
			tctx, cancel := context.WithTimeout(cctx, timeout)
			log.Info("turn_started", zap.Bool("resume", job.Resume != ""), zap.String("persona", job.Persona), zap.Duration("queued", waited))
			r.count(1)
			res = r.execute(tctx, tok, id, job, &ev)
			r.count(-1)
			release()
			switch {
			case res.Error == "":
			case errors.Is(context.Cause(tctx), errCutOff):
				res = turn.Result{Error: errCutOff.Error()}
			case errors.Is(tctx.Err(), context.DeadlineExceeded):
				res = turn.Result{Error: fmt.Sprintf("the turn timed out after %s", timeout)}
			}
			cancel()
		}
	}
	if ctx.Err() != nil {
		// The stream dropped: the room has failed the turn already.
		log.Warn("turn_abandoned", zap.Error(ctx.Err()))
		return
	}
	if errors.Is(context.Cause(cctx), errStopped) {
		// The room ended the turn already and would refuse its result (005-AC5).
		ended = errStopped.Error()
		log.Info("turn_stopped")
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	presp, err := r.d.roomRequest(pctx, http.MethodPost, r.daemonURL("turns/"+ids), tok, res)
	if err == nil {
		if presp.StatusCode/100 != 2 {
			err = apiError(presp)
		}
		_ = presp.Body.Close()
	}
	if err != nil {
		log.Warn("turn_result_refused", zap.Error(err))
		ended = "the room refused the result: " + err.Error()
		return
	}
	ended = res.Error
	log.Info("turn_done", zap.String("error", res.Error), zap.Int("changes", len(res.Changes)), zap.Int("session_bytes", len(res.Session)))
}

// slot waits for a slot under the host's global cap, and returns the function that frees it and
// how long the turn waited. While it waits, the room shows the turn "queued (host busy)" (003-AC18).
func (r *roomRun) slot(ctx context.Context, tok string, id int64) (func(), time.Duration, error) {
	if r.d.acquire == nil {
		return func() {}, 0, nil
	}
	start := time.Now()
	queued := false
	release, err := r.d.acquire(ctx, r.room.String(), id, func() {
		queued = true
		r.update(func(s *RoomStatus) { s.Queued++ })
		r.postState(ctx, tok, id, true)
	})
	if queued {
		r.update(func(s *RoomStatus) { s.Queued-- })
		if err == nil {
			r.postState(ctx, tok, id, false)
		}
	}
	return release, time.Since(start), err
}

// postState tells the room that the turn waits in this machine's queue, or runs now. A room server
// from before spec 003 phase 5 knows no such state: it shows the turn running throughout.
func (r *roomRun) postState(ctx context.Context, tok string, id int64, queued bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := r.d.roomRequest(ctx, http.MethodPost, r.daemonURL("turns/"+strconv.FormatInt(id, 10)+"/state"), tok, map[string]bool{"host_queued": queued})
	if err == nil {
		if resp.StatusCode/100 != 2 {
			err = apiError(resp)
		}
		_ = resp.Body.Close()
	}
	if err != nil {
		r.d.o.Log.Debug("turn_state_failed", zap.Int64("turn", id), zap.Bool("queued", queued), zap.Error(err))
	}
}

// count moves the room's count of running turns.
func (r *roomRun) count(n int) { r.update(func(s *RoomStatus) { s.Running += n }) }

// execute fetches the workspace, runs the turn in a free sandbox and shapes the result. It notes
// in ev the model and effort the turn ran with, its tokens, cost and workspace size.
func (r *roomRun) execute(ctx context.Context, tok string, id int64, job turn.Job, ev *TurnEvent) turn.Result {
	ids := strconv.FormatInt(id, 10)
	px, err := r.d.proxyFor(r.room.String())
	if err != nil {
		return turn.Result{Error: err.Error()}
	}
	ag, free, err := r.agent(px.Addr)
	if err != nil {
		return turn.Result{Error: err.Error()}
	}
	defer free()
	// Before the workspace: its tar holds the worker's cache lock until read, and a sandbox
	// start takes seconds that the other rooms' turns would wait.
	if err := ag.Start(ctx); err != nil {
		return turn.Result{Error: err.Error()}
	}
	// Before the workspace too, whose tar holds the cache lock.
	load, skipped, opts, redact := r.d.turnPluginsSetup(r.room.String(), job.Plugins)
	// An error may quote what a hook printed: the room, the turn event and the log get it redacted.
	red := sandbox.NewRedactor(redact)
	if len(skipped) > 0 {
		r.d.o.Log.Info("turn_plugins_skipped", zap.String("persona", job.Persona), zap.Any("plugins", skipped))
	}
	var plugins []sandbox.PluginTar
	if len(load) > 0 {
		var closePlugins func()
		if plugins, closePlugins, err = r.turnPlugins(ctx, tok, ids, load); err != nil {
			return turn.Result{Error: err.Error(), SkippedPlugins: skipped}
		}
		defer closePlugins()
		r.d.o.Log.Info("turn_plugins", zap.String("persona", job.Persona), zap.Any("plugins", load))
	}
	ws, err := r.workspace(ctx, tok, id)
	if err != nil {
		return turn.Result{Error: "could not fetch the workspace: " + err.Error()}
	}
	defer ws.Close()
	run, capped := r.d.runSettings(job)
	ev.Model, ev.Effort = run.Model, run.Effort
	if capped {
		r.d.o.Log.Info("turn_capped", zap.String("persona", job.Persona), zap.String("asked", turn.Level{Model: job.Model, Effort: job.Effort}.String()),
			zap.String("runs", run.String()))
	}
	var plugin io.Reader
	if len(job.Skills) > 0 {
		resp, err := r.get(ctx, tok, "turns/"+ids+"/skills")
		if err != nil {
			return turn.Result{Error: "could not fetch the persona's skills: " + err.Error()}
		}
		defer resp.Body.Close()
		plugin = resp.Body
		r.d.o.Log.Info("turn_skills", zap.String("persona", job.Persona), zap.Any("skills", job.Skills))
	}
	wsr := &countingReader{r: ws}
	ran := time.Now()
	out, err := ag.RunTurn(ctx, sandbox.TurnInput{
		Plugin: plugin, Plugins: plugins, PluginOptions: opts, Redact: redact, Bearer: px.Bearer, Autocompact: knownAutocompact(job.Autocompact),
		Workspace: wsr, Prompt: job.Prompt, System: job.System, Model: run.Model, Effort: run.Effort,
		Resume: job.Resume, SessionSize: job.SessionSize, SessionSHA256: job.SessionSHA256,
		FetchSession: func(ctx context.Context) ([]byte, error) {
			resp, err := r.get(ctx, tok, "sessions/"+job.Resume)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			return io.ReadAll(io.LimitReader(resp.Body, maxSession))
		},
	})
	ev.WorkspaceBytes = wsr.n.Load()
	blocked := r.readEgress(ctx, ag, ran, job.Persona)
	if err != nil {
		return turn.Result{Error: red.Text(err.Error()), SkippedPlugins: skipped, Blocked: blocked}
	}
	model, note, mismatch := out.Result.Label(run.Model)
	if mismatch {
		r.d.o.Log.Warn("turn_model_mismatch", zap.String("persona", job.Persona), zap.String("asked", model), zap.String("reported", note))
	}
	ev.Model, ev.ModelNote, ev.ModelMismatch, ev.CostUSD = model, note, mismatch, out.Result.TotalCostUSD
	ev.TokensIn, ev.TokensOut, ev.TokensCacheRead, ev.TokensCacheWrite = out.Result.Tokens()
	tokens, window := out.Result.Context()
	return turn.Result{Synthesis: out.Result.Result, Model: model, ModelNote: note, ModelMismatch: mismatch, Effort: run.Effort, Capped: capped,
		SessionID: out.Result.SessionID, SessionOffset: out.SessionOffset,
		Session: out.Session, Changes: out.Changes, ContextTokens: tokens, ContextWindow: window, SkippedPlugins: skipped, Blocked: blocked}
}

// maxManifest bounds the manifest a room sends: tens of thousands of files.
const maxManifest = 16 << 20

// workspace returns the turn's workspace tar. With a cache it is built from the room's cached
// files plus a tar of the ones the cache lacks (002-AC37). With no cache for the room, a corrupt
// cached file, a partial tar that differs from the manifest, or no manifest (an older room
// server), it is the full tar, which refills the cache (002-AC38).
func (r *roomRun) workspace(ctx context.Context, tok string, id int64) (io.ReadCloser, error) {
	ids := strconv.FormatInt(id, 10)
	c := r.d.cache
	if c == nil {
		resp, err := r.get(ctx, tok, "turns/"+ids+"/workspace")
		if err != nil {
			return nil, err
		}
		return resp.Body, nil
	}
	c.mu.Lock()
	ws, err := r.cachedWorkspace(ctx, tok, id, c)
	if err != nil {
		c.mu.Unlock()
	}
	return ws, err
}

// cachedWorkspace is workspace with c locked; the returned tar unlocks it once read or closed.
func (r *roomRun) cachedWorkspace(ctx context.Context, tok string, id int64, c *wsCache) (io.ReadCloser, error) {
	ids := strconv.FormatInt(id, 10)
	log := r.d.o.Log.With(zap.Int64("turn", id))
	dir := c.roomDir(r.room)
	done := func() { c.evict(); c.mu.Unlock() }
	m, reason, err := r.manifest(ctx, tok, ids)
	if reason == "" {
		if _, serr := os.Stat(dir); errors.Is(serr, fs.ErrNotExist) {
			reason = "missing"
		}
	}
	if reason == "" {
		var lack []turn.File
		lack, err = missing(dir, m.Files)
		switch {
		case errors.Is(err, errCorrupt):
			reason = "corrupt"
		case err != nil:
			return nil, err
		case len(lack) > 0:
			if err = r.fetchFiles(ctx, tok, ids, dir, lack); err != nil {
				reason = "partial"
			}
		}
	}
	if reason == "" {
		return assemble(dir, m.Dirs, m.Files, done), nil
	}
	log.Warn("workspace_fallback", zap.String("reason", reason), zap.Error(err))
	resp, err := r.get(ctx, tok, "turns/"+ids+"/workspace?fallback="+reason)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	dirs, files, err := ingest(dir, resp.Body, nil)
	if err != nil {
		return nil, err
	}
	return assemble(dir, dirs, files, done), nil
}

// manifest fetches the turn's manifest; a reason is set when there is none to use.
func (r *roomRun) manifest(ctx context.Context, tok, ids string) (*turn.Manifest, string, error) {
	resp, err := r.get(ctx, tok, "turns/"+ids+"/manifest")
	if err != nil {
		return nil, "manifest", err
	}
	defer resp.Body.Close()
	var m turn.Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxManifest)).Decode(&m); err != nil {
		return nil, "manifest", fmt.Errorf("manifest: %w", err)
	}
	if err := validManifest(&m); err != nil {
		return nil, "manifest", err
	}
	return &m, "", nil
}

// fetchFiles asks the room for a tar of files only and stores them in dir.
func (r *roomRun) fetchFiles(ctx context.Context, tok, ids, dir string, files []turn.File) error {
	req := turn.PathsRequest{Paths: make([]string, len(files))}
	want := make(map[string]turn.File, len(files))
	for i, f := range files {
		req.Paths[i] = f.Path
		want[f.Path] = f
	}
	resp, err := r.d.roomRequest(ctx, http.MethodPost, r.daemonURL("turns/"+ids+"/workspace"), tok, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	_, _, err = ingest(dir, resp.Body, want)
	return err
}

// countingReader counts the bytes read through it; atomic, as the sandbox may read from another
// goroutine.
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// readEgress logs the egress requests of the turn that ran in ag since since (014-AC3) and returns
// the domains it was refused (014-AC8). The turn's context may be over: the read gets its own
// deadline.
func (r *roomRun) readEgress(ctx context.Context, ag Agent, since time.Time, persona string) []string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	reqs, err := ag.Requests(ctx, since)
	if err != nil {
		r.d.o.Log.Warn("egress_read_failed", zap.String("room", r.room.String()), zap.Error(err))
		return nil
	}
	r.d.logEgress(r.room.String(), persona, reqs)
	return blockedDomains(reqs)
}
