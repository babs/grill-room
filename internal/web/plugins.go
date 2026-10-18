package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/plugin"
	"github.com/babs/grill-room/internal/store"
)

const (
	// maxPluginFetches bounds the git fetches the server runs at once: each holds up to a few
	// times the snapshot cap in memory.
	maxPluginFetches = 2
	// maxValidateJSON bounds the claude plugin validate output a daemon reports.
	maxValidateJSON = 64 << 10
	// maxPluginError bounds the first validation error kept as the reason.
	maxPluginError = 500
	// maxReportDomains bounds the domains a report lists off the allowlist.
	maxReportDomains = 100
)

func pluginID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("plugin"), 10, 64)
	return id
}

// versionView is a plugin version as the room sees it. Change and CompareURL describe a version
// newer than the running one against it (009-AC15).
type versionView struct {
	store.PluginVersion
	ShortCommit string         `json:"short_commit"`
	CommitURL   string         `json:"commit_url,omitempty"`
	Change      *plugin.Change `json:"change,omitempty"`
	CompareURL  string         `json:"compare_url,omitempty"`
}

// pluginView is a room plugin as the room sees it: Running is the version turns load, 0 for none,
// and Why says why it does not load when it does not (009-AC18), a running version included when
// it needs host setup (009-AC24).
type pluginView struct {
	store.RoomPlugin
	Versions []versionView `json:"versions"`
	Running  int           `json:"running,omitempty"`
	Why      string        `json:"why,omitempty"`
}

func manifestOf(raw json.RawMessage) *plugin.Manifest {
	var m plugin.Manifest
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return &m
}

func filesOf(raw json.RawMessage) []plugin.File {
	var fs []plugin.File
	_ = json.Unmarshal(raw, &fs) // stored by the server from a snapshot: valid
	return fs
}

// waitingText is what a version waiting for inspection shows (009-AC13).
func waitingText(hostOnline bool) string {
	if hostOnline {
		return "the AI host is checking it"
	}
	return "waiting for the AI host to check it"
}

// viewPlugin describes p, with hostOnline telling whether the room's host app is connected.
func viewPlugin(p store.RoomPlugin, hostOnline bool) pluginView {
	v := pluginView{RoomPlugin: p, Versions: make([]versionView, len(p.Versions))}
	run := p.Running()
	if run != nil {
		v.Running = run.N
	}
	for i, pv := range p.Versions {
		if pv.State == store.PluginInspecting {
			pv.Reason = waitingText(hostOnline)
		}
		// An imported version's commit is the archive's word: no link vouches for its files (009-AC27).
		vv := versionView{PluginVersion: pv, ShortCommit: pv.Commit[:min(7, len(pv.Commit))]}
		if !pv.Imported {
			vv.CommitURL = plugin.CommitURL(p.URL, pv.Commit)
		}
		if run != nil && pv.N > run.N {
			c := plugin.Diff(filesOf(run.FileList), filesOf(pv.FileList), manifestOf(run.Manifest), manifestOf(pv.Manifest))
			vv.Change = &c
			if !pv.Imported && !run.Imported {
				vv.CompareURL = plugin.CompareURL(p.URL, run.Commit, pv.Commit)
			}
		}
		v.Versions[i] = vv
	}
	if run == nil && len(v.Versions) > 0 {
		switch last := v.Versions[len(v.Versions)-1]; last.State {
		case store.PluginInspecting, store.PluginInvalid:
			v.Why = last.Reason
		case store.PluginPending:
			v.Why = "waiting for the AI host's approval"
		case store.PluginRefused:
			v.Why = "refused by the AI host"
		}
	}
	if p.NeedsSetup {
		v.Why = needsSetupText
	}
	return v
}

// needsSetupText is why a plugin whose required option the host has not set does not load
// (009-AC24).
const needsSetupText = "needs host setup on the AI host's computer"

// pluginViews lists the room's plugins as the room sees them.
func (s *Server) pluginViews(ctx context.Context, room string) ([]pluginView, error) {
	ps, err := s.bus.Store.RoomPlugins(ctx, room)
	if err != nil {
		return nil, err
	}
	r, err := s.bus.Store.Room(ctx, room)
	if err != nil {
		return nil, err
	}
	online := r.HostParticipantID != 0 && s.onlineHosts()[room][r.HostParticipantID]
	out := make([]pluginView, len(ps))
	for i, p := range ps {
		out[i] = viewPlugin(p, online)
	}
	return out, nil
}

// listPlugins lists the room's plugins with their versions and personas, for every member (009-AC5).
func (s *Server) listPlugins(w http.ResponseWriter, r *http.Request, sess session) {
	ps, err := s.pluginViews(r.Context(), sess.p.RoomID)
	if err != nil {
		s.serverError(w, "list_plugins_failed", err)
		return
	}
	fs, err := s.bus.Store.PluginFetches(r.Context(), sess.p.RoomID, false)
	if err != nil {
		s.serverError(w, "list_plugins_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": ps, "fetches": fs})
}

// pluginReq is a repo to check or add from.
type pluginReq struct {
	URL      string   `json:"url"`
	Ref      string   `json:"ref"`
	Entries  []string `json:"entries"`
	Personas []int64  `json:"personas"`
}

// fetched is a repo's tree at a resolved commit.
type fetched struct {
	commit, refName string
	tree            []plugin.Entry
	maxBytes        int64
}

// fetchError strips the URL net/http puts in its errors: logs never hold a repo link (009-AC7).
func fetchError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return "git: " + ue.Err.Error()
	}
	return err.Error()
}

// hostFetch answers that the server cannot fetch the repo: the room may ask the AI host (009-AC19).
func hostFetch(w http.ResponseWriter, reason string) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": reason, "host_fetch": true, "reason": reason})
}

// fetchSlot takes one of the server's fetches, then the room's one, across pods; release gives
// both back. The server's first: it bounds the connections the room locks pin. It answers the
// request itself when it fails.
func (s *Server) fetchSlot(ctx context.Context, w http.ResponseWriter, room string) (release func(), ok bool) {
	select {
	case s.pluginFetches <- struct{}{}:
	default:
		httpError(w, http.StatusServiceUnavailable, "too many plugin fetches in progress, retry in a moment")
		return nil, false
	}
	free, ok, err := s.bus.Store.LockRoom(ctx, store.LockPlugin, room, true)
	switch {
	case err != nil:
		<-s.pluginFetches
		s.serverError(w, "plugin_fetch_failed", err)
		return nil, false
	case !ok:
		<-s.pluginFetches
		httpError(w, http.StatusConflict, "a plugin fetch already runs in this room")
		return nil, false
	}
	return func() { free(); <-s.pluginFetches }, true
}

// fetchRepo resolves ref in repo and fetches its tree, unless it resolves to known: then upToDate.
// It answers the request itself when it fails. The URL is checked first and never logged. On
// success the caller holds the room's fetch slot until it calls release: the tree and its
// snapshots stay in memory until the store has them.
func (s *Server) fetchRepo(w http.ResponseWriter, r *http.Request, room, repo, ref, known string) (f *fetched, release func(), upToDate, ok bool) {
	if err := plugin.CheckURL(repo); err != nil {
		switch {
		case errors.Is(err, plugin.ErrCredentials):
			s.log.Info("plugin_url_refused", zap.String("room", room), zap.String("why", "credentials"))
			httpError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, plugin.ErrNotHTTPS) && (strings.HasPrefix(strings.ToLower(repo), "ssh://") || plugin.IsSCP(repo)):
			hostFetch(w, "the server fetches https links only")
		default:
			httpError(w, http.StatusBadRequest, err.Error())
		}
		return nil, nil, false, false
	}
	slot, ok := s.fetchSlot(r.Context(), w, room)
	if !ok {
		return nil, nil, false, false
	}
	done := false
	defer func() {
		if !done {
			slot()
		}
	}()
	set, err := s.bus.Store.InstanceSettings(r.Context())
	if err != nil {
		s.serverError(w, "plugin_settings_failed", err)
		return nil, nil, false, false
	}
	// One deadline for the whole fetch (009-AC8).
	ctx, cancel := context.WithTimeout(r.Context(), plugin.Timeout)
	defer cancel()
	f = &fetched{maxBytes: int64(set.PluginMaxMB) << 20}
	if f.commit, f.refName, err = plugin.Resolve(ctx, s.opts.PluginClient, repo, ref); err == nil {
		if f.commit == known {
			return nil, nil, true, true
		}
		f.tree, err = plugin.Fetch(ctx, s.opts.PluginClient, repo, f.commit, f.maxBytes)
	}
	if err != nil {
		s.log.Info("plugin_fetch_failed", zap.String("room", room), zap.String("class", errorClass(err)))
		if errors.Is(err, plugin.ErrNoAccess) {
			hostFetch(w, err.Error())
		} else {
			httpError(w, http.StatusUnprocessableEntity, fetchError(err))
		}
		return nil, nil, false, false
	}
	done = true
	return f, slot, false, true
}

// errorClass names what kind of failure a fetch met, for the log: an error may quote the repo's
// files or the server's answer.
func errorClass(err error) string {
	for _, c := range []struct {
		err  error
		name string
	}{{plugin.ErrNoAccess, "no_access"}, {plugin.ErrNoV2, "no_v2"}, {plugin.ErrTooBig, "too_big"}, {plugin.ErrBlocked, "blocked"},
		{plugin.ErrInvalid, "invalid"}, {context.DeadlineExceeded, "timeout"}} {
		if errors.Is(err, c.err) {
			return c.name
		}
	}
	return "other"
}

// candidate is one plugin a repo holds: a marketplace entry, or the repo itself.
type candidate struct {
	Entry       string           `json:"entry,omitempty"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Version     string           `json:"version,omitempty"`
	Pickable    bool             `json:"pickable"`
	Reason      string           `json:"reason,omitempty"`
	Size        int64            `json:"size,omitempty"`
	Files       int              `json:"files,omitempty"`
	Manifest    *plugin.Manifest `json:"manifest,omitempty"`
	snap        *plugin.Snapshot
}

// maxCandidates bounds the marketplace entries snapshotted: an entry rooted at the repo's root
// costs a pass over the whole tree.
// ponytail: later entries are listed but not pickable; snapshot on demand if marketplaces grow past it.
const maxCandidates = 50

// candidates lists the plugins of f: the marketplace's entries, or the repo as one plugin
// (009-AC6). A plugin whose snapshot fails is listed, not pickable, with why. Only the entries
// keep names hold their snapshot: the others are read and dropped, so memory holds one at a time.
func candidates(ctx context.Context, f *fetched, repo string, keep func(entry string) bool) (market string, out []candidate, err error) {
	m, err := plugin.ReadMarketplace(f.tree)
	if err != nil {
		return "", nil, err
	}
	snap := func(c *candidate, root string, manifest []byte) {
		s, err := plugin.FromTree(f.tree, root, manifest, f.maxBytes)
		if err == nil {
			c.Manifest, err = s.Manifest()
		}
		if err != nil {
			c.Pickable, c.Reason = false, err.Error()
			return
		}
		c.Name, c.Size, c.Files = c.Manifest.Name, s.Size, len(s.Files)
		if keep(c.Entry) {
			c.snap = s
		}
	}
	if m == nil {
		// Without a plugin.json, Claude Code names a plugin after its folder.
		name, _ := json.Marshal(map[string]string{"name": plugin.RepoName(repo)})
		c := candidate{Name: plugin.RepoName(repo), Pickable: true}
		snap(&c, "", name)
		return "", []candidate{c}, nil
	}
	out = []candidate{}
	for i, e := range m.Plugins {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		c := candidate{Entry: e.Name, Name: e.Name, Description: e.Description, Version: e.Version, Pickable: e.Pickable, Reason: e.Reason}
		switch {
		case e.Pickable && i >= maxCandidates:
			c.Pickable, c.Reason = false, fmt.Sprintf("only the first %d plugins of a marketplace can be added", maxCandidates)
		case e.Pickable:
			snap(&c, e.Root, e.PluginJSON())
		}
		out = append(out, c)
	}
	return m.Name, out, nil
}

// checkPlugin resolves and fetches a repo for a room admin, and lists what it holds: the plugins
// to pick, or why not (009-AC5, 009-AC6).
func (s *Server) checkPlugin(w http.ResponseWriter, r *http.Request, sess session) {
	if !s.asRoomAdmin(w, r, &sess) {
		return
	}
	var req pluginReq
	if !decode(w, r, &req) {
		return
	}
	repo := strings.TrimSpace(req.URL)
	f, release, _, ok := s.fetchRepo(w, r, sess.p.RoomID, repo, strings.TrimSpace(req.Ref), "")
	if !ok {
		return
	}
	defer release()
	market, cs, err := candidates(r.Context(), f, repo, func(string) bool { return false })
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	have, err := s.bus.Store.RoomPlugins(r.Context(), sess.p.RoomID)
	if err != nil {
		s.serverError(w, "check_plugin_failed", err)
		return
	}
	for i, c := range cs {
		if c.Pickable && slices.ContainsFunc(have, func(p store.RoomPlugin) bool { return p.Name == c.Name }) {
			cs[i].Pickable, cs[i].Reason = false, store.ErrPluginName.Error()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"commit": f.commit, "short_commit": f.commit[:7], "ref_name": f.refName,
		"commit_url": plugin.CommitURL(repo, f.commit), "marketplace": market, "plugins": cs})
}

// snapshotOf is what the store keeps of a candidate's snapshot.
func snapshotOf(commit string, c candidate) store.PluginSnapshot {
	files, _ := json.Marshal(c.snap.Files)  // plain structs: cannot fail
	manifest, _ := json.Marshal(c.Manifest) // idem
	return store.PluginSnapshot{Commit: commit, SHA256: c.snap.SHA256, Size: c.snap.Size, Files: len(c.snap.Files),
		FileList: files, Manifest: manifest, Data: c.snap.Tar}
}

// pluginStoreError answers the store's refusals of a plugin change.
func (s *Server) pluginStoreError(w http.ResponseWriter, event string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such plugin or persona")
	case errors.Is(err, store.ErrLimit), errors.Is(err, store.ErrPluginName), errors.Is(err, store.ErrPersonaPluginName),
		errors.Is(err, store.ErrPluginState):
		httpError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrRoomFull):
		httpError(w, http.StatusRequestEntityTooLarge, err.Error())
	default:
		s.serverError(w, event, err)
	}
}

// addPlugin adds the picked plugins of a repo to the room, pinned at the commit its ref resolves
// to now, on for the personas listed (009-AC5, 009-AC6, 009-AC10). A marketplace needs its
// entries named; a plain repo is the one plugin.
func (s *Server) addPlugin(w http.ResponseWriter, r *http.Request, sess session) {
	if !s.asRoomAdmin(w, r, &sess) {
		return
	}
	var req pluginReq
	if !decode(w, r, &req) {
		return
	}
	if len(req.Entries) > store.MaxRoomPlugins {
		httpError(w, http.StatusConflict, fmt.Sprintf("%v: at most %d plugins per room", store.ErrLimit, store.MaxRoomPlugins))
		return
	}
	repo, ref := strings.TrimSpace(req.URL), strings.TrimSpace(req.Ref)
	f, release, _, ok := s.fetchRepo(w, r, sess.p.RoomID, repo, ref, "")
	if !ok {
		return
	}
	defer release()
	market, cs, err := candidates(r.Context(), f, repo, func(e string) bool { return e == "" || slices.Contains(req.Entries, e) })
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	var add []store.NewPlugin
	pick := func(c candidate) bool {
		if !c.Pickable {
			httpError(w, http.StatusUnprocessableEntity, fmt.Sprintf("the plugin %s cannot be added: %s", c.Name, c.Reason))
			return false
		}
		add = append(add, store.NewPlugin{Name: c.Name, URL: repo, Ref: ref, RefName: f.refName, Entry: c.Entry, PluginSnapshot: snapshotOf(f.commit, c)})
		return true
	}
	if market == "" && len(cs) == 1 && cs[0].Entry == "" {
		if !pick(cs[0]) {
			return
		}
	} else {
		if len(req.Entries) == 0 {
			httpError(w, http.StatusBadRequest, "pick at least one plugin of the marketplace")
			return
		}
		for _, name := range slices.Compact(slices.Sorted(slices.Values(req.Entries))) {
			i := slices.IndexFunc(cs, func(c candidate) bool { return c.Entry == name })
			if i < 0 {
				httpError(w, http.StatusUnprocessableEntity, "the marketplace lists no plugin "+strconv.Quote(name))
				return
			}
			if !pick(cs[i]) {
				return
			}
		}
	}
	msgs, err := s.bus.Store.AddPlugins(r.Context(), sess.p.RoomID, add, req.Personas, sess.p, s.opts.MaxRoomBytes)
	if err != nil {
		s.pluginStoreError(w, "add_plugin_failed", err)
		return
	}
	if len(msgs) > 0 {
		s.bus.Notify(sess.p.RoomID)
	}
	for _, p := range add {
		s.log.Info("plugin_added", zap.String("room", sess.p.RoomID), zap.String("plugin", p.Name), zap.String("commit", p.Commit), zap.Int64("bytes", p.Size))
	}
	ps, err := s.pluginViews(r.Context(), sess.p.RoomID)
	if err != nil {
		s.serverError(w, "list_plugins_failed", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"plugins": ps})
}

// updatePlugin re-resolves a plugin's ref: the same commit is up to date, a new one makes a new
// version that waits for inspection while the approved one keeps running (009-AC11).
func (s *Server) updatePlugin(w http.ResponseWriter, r *http.Request, sess session) {
	if !s.asRoomAdmin(w, r, &sess) {
		return
	}
	p, err := s.bus.Store.RoomPlugin(r.Context(), sess.p.RoomID, pluginID(r))
	if err != nil {
		s.pluginStoreError(w, "update_plugin_failed", err)
		return
	}
	if p.Private {
		s.askHostUpdate(w, r, sess, p)
		return
	}
	// An imported latest version is fetched again even at the same commit: its files came from an
	// archive, the repo's replace them (009-AC27).
	known := p.Latest().Commit
	if p.Latest().Imported {
		known = ""
	}
	f, release, upToDate, ok := s.fetchRepo(w, r, sess.p.RoomID, p.URL, p.Ref, known)
	if !ok {
		return
	}
	if upToDate {
		writeJSON(w, http.StatusOK, map[string]bool{"up_to_date": true})
		return
	}
	defer release()
	_, cs, err := candidates(r.Context(), f, p.URL, func(e string) bool { return e == p.Entry })
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	i := slices.IndexFunc(cs, func(c candidate) bool { return c.Entry == p.Entry })
	switch {
	case i < 0:
		httpError(w, http.StatusUnprocessableEntity, "the repo no longer lists the plugin "+p.Name)
		return
	case !cs[i].Pickable:
		httpError(w, http.StatusUnprocessableEntity, fmt.Sprintf("the new version of %s cannot be added: %s", p.Name, cs[i].Reason))
		return
	case cs[i].Name != p.Name:
		httpError(w, http.StatusUnprocessableEntity, fmt.Sprintf("the plugin is named %s now: remove it and add it again", cs[i].Name))
		return
	}
	m, err := s.bus.Store.AddPluginVersion(r.Context(), sess.p.RoomID, p.ID, f.refName, snapshotOf(f.commit, cs[i]), sess.p, s.opts.MaxRoomBytes)
	if errors.Is(err, store.ErrUpToDate) {
		writeJSON(w, http.StatusOK, map[string]bool{"up_to_date": true})
		return
	}
	if err != nil {
		s.pluginStoreError(w, "update_plugin_failed", err)
		return
	}
	s.bus.Notify(m.RoomID)
	s.log.Info("plugin_updated", zap.String("room", sess.p.RoomID), zap.String("plugin", p.Name), zap.String("commit", f.commit))
	s.writePlugin(w, r, sess.p.RoomID, p.ID, http.StatusCreated)
}

func (s *Server) writePlugin(w http.ResponseWriter, r *http.Request, room string, id int64, code int) {
	ps, err := s.pluginViews(r.Context(), room)
	if err != nil {
		s.serverError(w, "list_plugins_failed", err)
		return
	}
	i := slices.IndexFunc(ps, func(p pluginView) bool { return p.ID == id })
	if i < 0 {
		httpError(w, http.StatusNotFound, "no such plugin")
		return
	}
	writeJSON(w, code, ps[i])
}

// removePlugin takes a plugin off the room: every persona's next turn runs without it (009-AC12).
func (s *Server) removePlugin(w http.ResponseWriter, r *http.Request, sess session) {
	if !s.asRoomAdmin(w, r, &sess) {
		return
	}
	m, err := s.bus.Store.RemovePlugin(r.Context(), sess.p.RoomID, pluginID(r), sess.p)
	if err != nil {
		s.pluginStoreError(w, "remove_plugin_failed", err)
		return
	}
	s.bus.Notify(m.RoomID)
	s.log.Info("plugin_removed", zap.String("room", sess.p.RoomID), zap.Int64("plugin", pluginID(r)))
	w.WriteHeader(http.StatusNoContent)
}

// setPersonaPlugin turns a plugin on (PUT) or off (DELETE) for a persona: a room admin's to do
// (009-AC10).
func (s *Server) setPersonaPlugin(w http.ResponseWriter, r *http.Request, sess session) {
	if !s.asRoomAdmin(w, r, &sess) {
		return
	}
	if err := s.bus.Store.SetPersonaPlugin(r.Context(), sess.p.RoomID, personaID(r), pluginID(r), r.Method == http.MethodPut); err != nil {
		s.pluginStoreError(w, "set_persona_plugin_failed", err)
		return
	}
	s.bus.Notify(sess.p.RoomID)
	w.WriteHeader(http.StatusNoContent)
}

// decidePluginVersion is the host's answer to a version waiting for their approval (009-AC14).
func (s *Server) decidePluginVersion(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		Approve bool `json:"approve"`
	}
	if !decode(w, r, &req) {
		return
	}
	room, ok := s.asApprover(w, r, sess)
	if !ok {
		return
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	m, err := s.bus.Store.DecidePluginVersion(r.Context(), room.ID, pluginID(r), n, req.Approve, sess.p)
	if errors.Is(err, store.ErrPluginState) {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	s.decided(w, sess, m, err)
}

// asHostDaemon answers 403 unless d is the room's host daemon: only it inspects (009-AC13).
func (s *Server) asHostDaemon(w http.ResponseWriter, r *http.Request, d *store.Daemon) bool {
	host, err := s.bus.Store.HostDaemon(r.Context(), d.RoomID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.serverError(w, "host_daemon_failed", err)
		return false
	}
	if host == nil || host.ID != d.ID {
		httpError(w, http.StatusForbidden, "only the room's AI host daemon can do this")
		return false
	}
	return true
}

func versionID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("version"), 10, 64)
	return id
}

func writeTar(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data) //nolint:gosec // a tar served as application/x-tar with nosniff, never rendered
}

// getPluginSnapshot hands the host daemon a version's snapshot to inspect (009-AC13).
func (s *Server) getPluginSnapshot(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	if !s.asHostDaemon(w, r, d) {
		return
	}
	data, err := s.bus.Store.PluginVersionData(r.Context(), d.RoomID, versionID(r))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such plugin version")
		return
	}
	if err != nil {
		s.serverError(w, "plugin_snapshot_failed", err)
		return
	}
	writeTar(w, data)
}

// pluginReport is what the host daemon found inspecting a version (009-AC13): Validate is
// `claude plugin validate --json`'s output, Details what it read from `claude plugin details`,
// Error the first validation error when OK is false.
type pluginReport struct {
	OK       bool            `json:"ok"`
	Validate json.RawMessage `json:"validate,omitempty"`
	Details  *plugin.Details `json:"details,omitempty"`
	Error    string          `json:"error,omitempty"`
	// AllowlistMissing are the domains of the version's MCP servers off the host's egress
	// allowlist (009-AC25); the approval sheet marks only those the manifest names.
	AllowlistMissing []string `json:"allowlist_missing,omitempty"`
}

// postPluginReport records the host daemon's inspection of a version (009-AC13).
func (s *Server) postPluginReport(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	var rep pluginReport
	if !decodeLimit(w, r, &rep, maxValidateJSON+64<<10) {
		return
	}
	if len(rep.Validate) > maxValidateJSON {
		httpError(w, http.StatusRequestEntityTooLarge, "validate output over 64 KiB")
		return
	}
	if !s.asHostDaemon(w, r, d) {
		return
	}
	rep.AllowlistMissing = rep.AllowlistMissing[:min(len(rep.AllowlistMissing), maxReportDomains)]
	// Claude Code's errors may span lines: the reason is one line of the chat.
	rep.Error, _ = cleanText(truncate(strings.Join(strings.Fields(rep.Error), " "), maxPluginError), maxPluginError, false)
	if !rep.OK && rep.Error == "" {
		rep.Error = "the validation failed"
	}
	raw, _ := json.Marshal(rep) // decoded from JSON just above: cannot fail
	id := versionID(r)
	m, state, err := s.bus.Store.ReportPluginVersion(r.Context(), d.RoomID, id, d.ID, rep.OK, raw, rep.Error)
	switch {
	case errors.Is(err, store.ErrForbidden):
		httpError(w, http.StatusForbidden, "only the host's daemon inspects plugins")
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such plugin version")
	case errors.Is(err, store.ErrPluginState):
		httpError(w, http.StatusConflict, "this version was inspected already")
	case err != nil:
		s.serverError(w, "plugin_report_failed", err)
	default:
		s.bus.Notify(m.RoomID)
		s.log.Info("plugin_inspected", zap.String("room", d.RoomID), zap.Int64("version", id), zap.String("state", state))
		writeJSON(w, http.StatusOK, map[string]string{"state": state})
	}
}

// getTurnPlugin streams the snapshot of a plugin version the turn loads, by its sha256 (009-AC17).
func (s *Server) getTurnPlugin(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	t, ok := s.daemonTurn(w, r, d)
	if !ok {
		return
	}
	data, err := s.bus.Store.TurnPluginData(r.Context(), d.RoomID, t.ID, r.PathValue("sha256"))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "this turn loads no such plugin")
		return
	}
	if err != nil {
		s.serverError(w, "turn_plugin_failed", err)
		return
	}
	writeTar(w, data)
}
