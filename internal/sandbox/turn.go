package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/babs/grill-room/internal/plugin"
	"github.com/babs/grill-room/internal/turn"
)

// MaxChangedBytes bounds the changed files a turn reads back for the room.
const MaxChangedBytes = 24 << 20

// TurnInput is one agent turn (docs/agent-workspace.md).
type TurnInput struct {
	// Workspace is the tar the room built; it replaces /workspace whole.
	Workspace io.Reader
	Prompt    string
	// System is the persona's text, appended to Claude Code's system prompt.
	System string
	Model  string
	Effort string
	// Resume, SessionSize and SessionSHA256 name the session to continue and the room's copy of it.
	Resume        string
	SessionSize   int64
	SessionSHA256 string
	// FetchSession returns the room's copy of Resume; called only when the local copy differs.
	FetchSession func(ctx context.Context) ([]byte, error)
	// Plugin is the tar of the plugin directory holding the persona's active skills, nil for none
	// (002-AC31).
	Plugin io.Reader
	// Plugins are the room plugins the turn loads, each in its own --plugin-dir after Plugin's
	// (009-AC17).
	Plugins []PluginTar
	// Bearer is the dummy token for the account's proxy (Request.Bearer).
	Bearer string
	// Autocompact is Request.Autocompact.
	Autocompact int
}

// PluginTar is a room plugin a turn loads. Name becomes its folder; Tar is its snapshot, which
// the caller checked with plugin.ValidateTar: regular files only, no path leaving the folder.
type PluginTar struct {
	Name string
	Tar  io.Reader
}

// TurnOutput is what a turn produced for the room.
type TurnOutput struct {
	Result  *Result
	Changes []turn.Change
	// Session holds the session jsonl from SessionOffset on.
	SessionOffset int64
	Session       []byte
}

// RunTurn resets the workspace from the room's tar, makes sure the local session matches the
// room's, runs claude -p, and returns the changed files and the new session bytes.
func (s *Sandbox) RunTurn(ctx context.Context, in TurnInput) (*TurnOutput, error) {
	s.turn.Lock()
	defer s.turn.Unlock()
	if err := s.prepare(ctx, in.Bearer); err != nil {
		return nil, err
	}
	// The agent owns every file, but may have taken its own write bit away.
	if _, err := s.run(ctx, nil, "exec", s.container(), "sh", "-c", "chmod -R u+w "+Workspace+" && find "+Workspace+" -mindepth 1 -delete"); err != nil {
		return nil, fmt.Errorf("clear workspace: %w", err)
	}
	if _, err := s.run(ctx, in.Workspace, "exec", "-i", s.container(), "tar", "-x", "-C", Workspace); err != nil {
		return nil, fmt.Errorf("unpack workspace: %w", err)
	}
	before, err := s.hashes(ctx)
	if err != nil {
		return nil, err
	}
	if in.Resume != "" {
		if err := s.syncSession(ctx, in); err != nil {
			return nil, err
		}
	}
	req := Request{Prompt: in.Prompt, System: in.System, Model: in.Model, Effort: in.Effort, Resume: in.Resume, Autocompact: in.Autocompact}
	if req.PluginDirs, err = s.loadPlugins(ctx, in.Plugin, in.Plugins); err != nil {
		return nil, err
	}
	res, err := s.runClaude(ctx, req)
	if err != nil {
		return nil, err
	}
	after, err := s.hashes(ctx)
	if err != nil {
		return nil, err
	}
	out := &TurnOutput{Result: res, Changes: diff(before, after)}
	if err := s.readChanged(ctx, out.Changes); err != nil {
		return nil, err
	}
	if res.SessionID == in.Resume {
		out.SessionOffset = in.SessionSize
	}
	if out.Session, err = s.ReadSession(ctx, res.SessionID, out.SessionOffset); err != nil {
		return nil, err
	}
	return out, nil
}

// pluginDir holds the turn's skills and pluginsDir its room plugins, one folder each, in the
// sandbox home, out of the workspace the agent edits.
const (
	pluginDir  = home + "/.grill-room/plugin"
	pluginsDir = home + "/.grill-room/plugins"
)

// loadPlugins replaces the skills' plugin directory and the room plugins' folders with the turn's,
// and returns them for --plugin-dir, the skills' first. Nothing of a previous turn carries over
// (002-AC31, 009-AC17).
func (s *Sandbox) loadPlugins(ctx context.Context, skills io.Reader, plugins []PluginTar) ([]string, error) {
	if _, err := s.run(ctx, nil, "exec", s.container(), "rm", "-rf", pluginDir, pluginsDir); err != nil {
		return nil, fmt.Errorf("clear plugins: %w", err)
	}
	var dirs []string
	var tars []io.Reader
	if skills != nil {
		dirs, tars = append(dirs, pluginDir), append(tars, skills)
	}
	for _, p := range plugins {
		// The name becomes a path: checked here, whatever the caller did.
		if err := plugin.CheckName(p.Name); err != nil {
			return nil, err
		}
		dir := pluginsDir + "/" + p.Name
		if slices.Contains(dirs, dir) {
			return nil, fmt.Errorf("two plugins named %s", p.Name)
		}
		dirs, tars = append(dirs, dir), append(tars, p.Tar)
	}
	if len(dirs) == 0 {
		return nil, nil
	}
	if _, err := s.run(ctx, nil, append([]string{"exec", s.container(), "mkdir", "-p"}, dirs...)...); err != nil {
		return nil, fmt.Errorf("load plugins: %w", err)
	}
	for i, dir := range dirs {
		if _, err := s.run(ctx, tars[i], "exec", "-i", s.container(), "tar", "-x", "-C", dir); err != nil {
			return nil, fmt.Errorf("load plugin %s: %w", path.Base(dir), err)
		}
	}
	return dirs, nil
}

func sessionPath(id string) string {
	return home + "/.claude/projects/" + strings.ReplaceAll(Workspace, "/", "-") + "/" + id + ".jsonl"
}

// syncSession replaces the local copy of the session with the room's unless it already is the
// same bytes: the last turn may have run on another daemon, or failed here after claude appended.
func (s *Sandbox) syncSession(ctx context.Context, in TurnInput) error {
	if !sessionIDRe.MatchString(in.Resume) {
		return fmt.Errorf("session id %q is not a UUID", in.Resume)
	}
	out, err := s.run(ctx, nil, "exec", s.container(), "sh", "-c", `stat -c %s "$1" && sha256sum < "$1"`, "sh", sessionPath(in.Resume))
	if err == nil {
		size, sum, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		if size == strconv.FormatInt(in.SessionSize, 10) && strings.HasPrefix(sum, in.SessionSHA256+" ") {
			return nil
		}
	}
	data, err := in.FetchSession(ctx)
	if err != nil {
		return fmt.Errorf("fetch session %s: %w", in.Resume, err)
	}
	if got := sha256.Sum256(data); int64(len(data)) != in.SessionSize || hex.EncodeToString(got[:]) != in.SessionSHA256 {
		return fmt.Errorf("session %s from the room does not match its announced size and hash", in.Resume)
	}
	dir := strings.TrimPrefix(sessionPath(in.Resume), home+"/")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()
	parts := strings.Split(dir, "/")
	for i := 1; i < len(parts); i++ {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: strings.Join(parts[:i], "/") + "/", Mode: 0o700, ModTime: now}); err != nil {
			return fmt.Errorf("seed session: %w", err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: dir, Mode: 0o600, Size: int64(len(data)), ModTime: now}); err != nil {
		return fmt.Errorf("seed session: %w", err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("seed session: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("seed session: %w", err)
	}
	if _, err := s.run(ctx, &buf, "exec", "-i", s.container(), "tar", "-x", "-C", home); err != nil {
		return fmt.Errorf("seed session: %w", err)
	}
	return nil
}

// hashes returns the sha256 of every regular file under /workspace, by relative path.
func (s *Sandbox) hashes(ctx context.Context) (map[string]string, error) {
	out, err := s.run(ctx, nil, "exec", s.container(), "sh", "-c", "cd "+Workspace+" && find . -type f -print0 | xargs -0 -r sha256sum -z")
	if err != nil {
		return nil, fmt.Errorf("hash workspace: %w", err)
	}
	return parseHashes(out)
}

// parseHashes reads sha256sum -z output: "<hex>  ./<path>" records ended by NUL, so any file name
// survives.
func parseHashes(out []byte) (map[string]string, error) {
	m := map[string]string{}
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		sum, p, ok := strings.Cut(rec, "  ")
		if !ok || len(sum) != 64 || !strings.HasPrefix(p, "./") {
			return nil, fmt.Errorf("hash workspace: unexpected line %q", rec)
		}
		m[strings.TrimPrefix(p, "./")] = sum
	}
	return m, nil
}

// diff lists created, modified and deleted paths, sorted.
func diff(before, after map[string]string) []turn.Change {
	var out []turn.Change
	for p, sum := range after {
		if before[p] != sum {
			out = append(out, turn.Change{Path: p})
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			out = append(out, turn.Change{Path: p, Deleted: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// readChanged fills in the content of the changes the room needs to see.
func (s *Sandbox) readChanged(ctx context.Context, changes []turn.Change) error {
	want := map[string]int{}
	var list bytes.Buffer
	for i, c := range changes {
		if !c.Deleted && turn.NeedsContent(c.Path) {
			want[c.Path] = i
			list.WriteString(c.Path + "\x00")
		}
	}
	if len(want) == 0 {
		return nil
	}
	out, err := s.run(ctx, &list, "exec", "-i", s.container(), "tar", "-c", "-C", Workspace, "--null", "-T", "-")
	if err != nil {
		return fmt.Errorf("read changed files: %w", err)
	}
	tr := tar.NewReader(bytes.NewReader(out))
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read changed files: %w", err)
		}
		i, ok := want[h.Name]
		// A file swapped for a link after hashing is not a document.
		if !ok || h.Typeflag != tar.TypeReg {
			continue
		}
		if total += h.Size; total > MaxChangedBytes {
			return fmt.Errorf("the turn changed over %d MB of documents", MaxChangedBytes>>20)
		}
		if changes[i].Content, err = io.ReadAll(tr); err != nil {
			return fmt.Errorf("read changed files: %w", err)
		}
	}
}
