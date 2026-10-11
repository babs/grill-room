package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/turn"
)

// Agent runs turns in a sandbox; *sandbox.Sandbox is the real one.
type Agent interface {
	RunTurn(ctx context.Context, in sandbox.TurnInput) (*sandbox.TurnOutput, error)
	// SetAllowlist sets the egress allowlist; a running sandbox takes it at once.
	SetAllowlist(ctx context.Context, list []string) error
	// Denied returns the last n refused egress requests, newest first.
	Denied(ctx context.Context, n int) ([]sandbox.Denied, error)
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

// runSettings is the one place where the model and effort a persona asks for become the ones its
// turn runs with. The host cap, raised by what the host approved for the persona, clamps them
// (002-AC19, 002-AC20); an empty model then falls back to this daemon's.
func (d *Daemon) runSettings(job turn.Job) (run turn.Level, capped bool) {
	run, capped = turn.Clamp(known(turn.Level{Model: job.Model, Effort: job.Effort}), d.Status().Settings.Limit(), known(job.Approved))
	if run.Model == "" {
		run.Model = d.o.Model
	}
	return run, capped
}

// agent returns the room run's sandbox, created on its first turn.
func (r *roomRun) agent() (Agent, error) {
	r.agentMu.Lock()
	defer r.agentMu.Unlock()
	if r.ag == nil {
		img := r.d.Status().Image
		if img.State != "ready" {
			return nil, fmt.Errorf("the sandbox image is not ready on %s", r.d.o.Name)
		}
		ag := r.d.o.NewAgent(r.room, img.Ref)
		// Under agentMu, read after any saved list: SetAllowlist hands later ones to r.ag.
		if err := ag.SetAllowlist(r.ctx, r.d.Status().Allowlist); err != nil {
			return nil, fmt.Errorf("egress allowlist: %w", err)
		}
		r.ag = ag
	}
	return r.ag, nil
}

// closeAgent removes the sandbox when the room run ends.
func (r *roomRun) closeAgent(ctx context.Context) {
	r.agentMu.Lock()
	defer r.agentMu.Unlock()
	if r.ag != nil {
		r.ag.Close(ctx)
		r.ag = nil
	}
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
	log := r.d.o.Log.With(zap.Int64("turn", id))
	ids := strconv.FormatInt(id, 10)
	jctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	resp, err := r.get(jctx, tok, "turns/"+ids)
	var job turn.Job
	if err == nil {
		err = json.NewDecoder(resp.Body).Decode(&job)
		_ = resp.Body.Close()
	}
	cancel()
	var res turn.Result
	if err != nil {
		log.Warn("turn_job_failed", zap.Error(err))
		// Told now, the room frees the button instead of waiting for its timeout.
		res.Error = "could not fetch the turn on " + r.d.o.Name + ": " + err.Error()
	} else {
		timeout := time.Duration(job.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 15 * time.Minute
		}
		tctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		log.Info("turn_started", zap.Bool("resume", job.Resume != ""), zap.String("persona", job.Persona))
		res = r.execute(tctx, tok, ids, job)
		if res.Error != "" && errors.Is(tctx.Err(), context.DeadlineExceeded) {
			res = turn.Result{Error: fmt.Sprintf("the turn timed out after %s on %s", timeout, r.d.o.Name)}
		}
	}
	if ctx.Err() != nil {
		// The stream dropped: the room has failed the turn already.
		log.Warn("turn_abandoned", zap.Error(ctx.Err()))
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
		return
	}
	log.Info("turn_done", zap.String("error", res.Error), zap.Int("changes", len(res.Changes)), zap.Int("session_bytes", len(res.Session)))
}

// execute fetches the workspace, runs the turn in the sandbox and shapes the result.
func (r *roomRun) execute(ctx context.Context, tok, ids string, job turn.Job) turn.Result {
	ws, err := r.get(ctx, tok, "turns/"+ids+"/workspace")
	if err != nil {
		return turn.Result{Error: "could not fetch the workspace on " + r.d.o.Name + ": " + err.Error()}
	}
	defer ws.Body.Close()
	ag, err := r.agent()
	if err != nil {
		return turn.Result{Error: err.Error()}
	}
	run, capped := r.d.runSettings(job)
	if capped {
		r.d.o.Log.Info("turn_capped", zap.String("persona", job.Persona), zap.String("asked", turn.Level{Model: job.Model, Effort: job.Effort}.String()),
			zap.String("runs", run.String()))
	}
	var plugin io.Reader
	if len(job.Skills) > 0 {
		resp, err := r.get(ctx, tok, "turns/"+ids+"/skills")
		if err != nil {
			return turn.Result{Error: "could not fetch the persona's skills on " + r.d.o.Name + ": " + err.Error()}
		}
		defer resp.Body.Close()
		plugin = resp.Body
		r.d.o.Log.Info("turn_skills", zap.String("persona", job.Persona), zap.Any("skills", job.Skills))
	}
	model := run.Model
	out, err := ag.RunTurn(ctx, sandbox.TurnInput{
		Plugin:    plugin,
		Workspace: ws.Body, Prompt: job.Prompt, System: job.System, Model: model, Effort: run.Effort,
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
	if err != nil {
		return turn.Result{Error: err.Error()}
	}
	// The model claude reports is the one that answered; the one asked for is a fallback.
	if m := out.Result.Model(); m != "" {
		model = m
	}
	return turn.Result{Synthesis: out.Result.Result, Model: model, Effort: run.Effort, Capped: capped, SessionID: out.Result.SessionID, SessionOffset: out.SessionOffset,
		Session: out.Session, Changes: out.Changes}
}
