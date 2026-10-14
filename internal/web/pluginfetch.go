package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/plugin"
	"github.com/babs/grill-room/internal/store"
)

// maxFetchReason bounds the reason a daemon gives for a failed fetch.
const maxFetchReason = 300

var commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func fetchID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("fetch"), 10, 64)
	return id
}

// checkFetchReq validates what a room admin asks the AI host to fetch; the error is the answer.
func checkFetchReq(repo, ref string, entries []string) error {
	if err := plugin.CheckHostURL(repo); err != nil {
		return err
	}
	if err := plugin.CheckRef(ref); err != nil {
		return err
	}
	if len(entries) > store.MaxRoomPlugins {
		return fmt.Errorf("%w: at most %d plugins per room", store.ErrLimit, store.MaxRoomPlugins)
	}
	for _, e := range entries {
		if err := plugin.CheckName(e); err != nil {
			return err
		}
	}
	return nil
}

// fetchStoreError answers the store's refusals of a fetch request.
func (s *Server) fetchStoreError(w http.ResponseWriter, event string, err error) {
	switch {
	case errors.Is(err, store.ErrFetchWaiting), errors.Is(err, store.ErrPluginState):
		httpError(w, http.StatusConflict, err.Error())
	default:
		s.pluginStoreError(w, event, err)
	}
}

// askHostFetch asks the room's AI host to fetch a repo the server cannot read: a private one, or
// an ssh link (009-AC19). Nothing is fetched until the host presses Fetch on its daemon page.
func (s *Server) askHostFetch(w http.ResponseWriter, r *http.Request, sess session) {
	if !s.asRoomAdmin(w, r, &sess) {
		return
	}
	var req pluginReq
	if !decode(w, r, &req) {
		return
	}
	repo, ref := strings.TrimSpace(req.URL), strings.TrimSpace(req.Ref)
	entries := slices.Compact(slices.Sorted(slices.Values(req.Entries)))
	if err := checkFetchReq(repo, ref, entries); err != nil {
		if errors.Is(err, plugin.ErrCredentials) {
			s.log.Info("plugin_url_refused", zap.String("room", sess.p.RoomID), zap.String("why", "credentials"))
		}
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	f, m, err := s.bus.Store.AddPluginFetch(r.Context(), sess.p.RoomID, store.NewPluginFetch{URL: repo, Ref: ref, Entries: entries, Personas: req.Personas}, sess.p)
	if err != nil {
		s.fetchStoreError(w, "plugin_fetch_request_failed", err)
		return
	}
	s.bus.Notify(m.RoomID)
	s.log.Info("plugin_fetch_requested", zap.String("room", sess.p.RoomID), zap.Int64("fetch", f.ID))
	writeJSON(w, http.StatusAccepted, f)
}

// askHostUpdate asks the AI host to fetch the update of a private plugin (009-AC21).
func (s *Server) askHostUpdate(w http.ResponseWriter, r *http.Request, sess session, p *store.RoomPlugin) {
	var entries []string
	if p.Entry != "" {
		entries = []string{p.Entry}
	}
	f, m, err := s.bus.Store.AddPluginFetch(r.Context(), sess.p.RoomID, store.NewPluginFetch{PluginID: p.ID, URL: p.URL, Ref: p.Ref, Entries: entries}, sess.p)
	if err != nil {
		s.fetchStoreError(w, "plugin_fetch_request_failed", err)
		return
	}
	s.bus.Notify(m.RoomID)
	s.log.Info("plugin_fetch_requested", zap.String("room", sess.p.RoomID), zap.Int64("fetch", f.ID), zap.String("plugin", p.Name))
	writeJSON(w, http.StatusAccepted, map[string]any{"host_fetch": f})
}

// deletePluginFetch cancels a request still waiting for the AI host, or dismisses a failed one.
func (s *Server) deletePluginFetch(w http.ResponseWriter, r *http.Request, sess session) {
	if !s.asRoomAdmin(w, r, &sess) {
		return
	}
	if err := s.bus.Store.DeletePluginFetch(r.Context(), sess.p.RoomID, fetchID(r)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusNotFound, "no such request")
			return
		}
		s.serverError(w, "delete_plugin_fetch_failed", err)
		return
	}
	s.bus.Notify(sess.p.RoomID)
	w.WriteHeader(http.StatusNoContent)
}

// daemonPlugin is a room plugin as the host's daemon needs it: the options its page asks the host
// for and the domains its MCP servers name (009-AC22, 009-AC25). Never an option value.
type daemonPlugin struct {
	ID      int64           `json:"id"`
	Name    string          `json:"name"`
	Version int             `json:"version"`
	Options []plugin.Option `json:"options,omitempty"`
	Domains []string        `json:"domains,omitempty"`
}

// daemonPlugins lists the room's plugins and the fetches waiting for the host, for the host's
// daemon page (009-AC19, 009-AC22). A plugin is described by its running version, else its latest.
func (s *Server) daemonPlugins(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	if !s.asHostDaemon(w, r, d) {
		return
	}
	ps, err := s.bus.Store.RoomPlugins(r.Context(), d.RoomID)
	if err != nil {
		s.serverError(w, "daemon_plugins_failed", err)
		return
	}
	fs, err := s.bus.Store.PluginFetches(r.Context(), d.RoomID, true)
	if err != nil {
		s.serverError(w, "daemon_plugins_failed", err)
		return
	}
	set, err := s.bus.Store.InstanceSettings(r.Context())
	if err != nil {
		s.serverError(w, "daemon_plugins_failed", err)
		return
	}
	out := make([]daemonPlugin, 0, len(ps))
	for _, p := range ps {
		v := p.Running()
		if v == nil {
			v = p.Latest()
		}
		dp := daemonPlugin{ID: p.ID, Name: p.Name}
		if v != nil {
			dp.Version = v.N
			if m := manifestOf(v.Manifest); m != nil {
				dp.Options, dp.Domains = m.Options, m.Domains
			}
		}
		out = append(out, dp)
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": out, "fetches": fs, "max_bytes": int64(set.PluginMaxMB) << 20})
}

// fetchFailure is a daemon's word that it did not fetch: the host declined, or git failed.
type fetchFailure struct {
	Error    string `json:"error"`
	Declined bool   `json:"declined"`
}

// postPluginFetch takes what the host's daemon fetched for a request, or why it did not (009-AC20,
// 009-AC21). A fetch comes as multipart/form-data: the commit, the ref's name, and one snapshot tar
// per plugin, named after its marketplace entry, "." for a repo that is one plugin. The server
// checks each tar against the AC9 rules again, and the request's plugins against what was asked.
func (s *Server) postPluginFetch(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	if !s.asHostDaemon(w, r, d) {
		return
	}
	f, err := s.bus.Store.PluginFetch(r.Context(), d.RoomID, fetchID(r))
	if err != nil {
		s.fetchStoreError(w, "plugin_fetch_failed", err)
		return
	}
	if f.State != store.FetchWaiting {
		httpError(w, http.StatusConflict, store.ErrPluginState.Error())
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "application/json" {
		var ff fetchFailure
		if !decode(w, r, &ff) {
			return
		}
		reason := "the AI host declined it"
		if !ff.Declined {
			reason = sanitize(strings.Join(strings.Fields(ff.Error), " "), maxFetchReason)
			if reason == "" {
				reason = "the fetch failed"
			}
		}
		s.failFetch(r.Context(), w, d.RoomID, f.ID, reason, http.StatusNoContent)
		return
	}
	set, err := s.bus.Store.InstanceSettings(r.Context())
	if err != nil {
		s.serverError(w, "plugin_fetch_failed", err)
		return
	}
	maxBytes := int64(set.PluginMaxMB) << 20
	ps, why, err := readFetched(w, r, f, maxBytes)
	if err != nil {
		s.serverError(w, "plugin_fetch_failed", err)
		return
	}
	if why != "" {
		s.failFetch(r.Context(), w, d.RoomID, f.ID, why, http.StatusUnprocessableEntity)
		return
	}
	if f.PluginID != 0 {
		p, err := s.bus.Store.RoomPlugin(r.Context(), d.RoomID, f.PluginID)
		switch {
		case err != nil:
			s.fetchStoreError(w, "plugin_fetch_failed", err)
			return
		case ps[0].Name != p.Name:
			s.failFetch(r.Context(), w, d.RoomID, f.ID, fmt.Sprintf("the plugin is named %s now: remove it and add it again", ps[0].Name), http.StatusUnprocessableEntity)
			return
		}
	}
	msgs, upToDate, err := s.bus.Store.CompletePluginFetch(r.Context(), d.RoomID, f.ID, ps, s.opts.MaxRoomBytes)
	switch {
	case errors.Is(err, store.ErrLimit), errors.Is(err, store.ErrPluginName), errors.Is(err, store.ErrPersonaPluginName), errors.Is(err, store.ErrRoomFull):
		s.failFetch(r.Context(), w, d.RoomID, f.ID, err.Error(), http.StatusConflict)
		return
	case err != nil:
		s.fetchStoreError(w, "plugin_fetch_failed", err)
		return
	}
	if len(msgs) > 0 {
		s.bus.Notify(d.RoomID)
	}
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.Name
	}
	s.log.Info("plugin_fetched", zap.String("room", d.RoomID), zap.Int64("fetch", f.ID), zap.Strings("plugins", names),
		zap.String("commit", ps[0].Commit), zap.Bool("up_to_date", upToDate))
	writeJSON(w, http.StatusCreated, map[string]any{"plugins": names, "up_to_date": upToDate})
}

// failFetch marks request id failed with reason and answers code.
func (s *Server) failFetch(ctx context.Context, w http.ResponseWriter, room string, id int64, reason string, code int) {
	m, err := s.bus.Store.FailPluginFetch(context.WithoutCancel(ctx), room, id, reason)
	if err != nil {
		s.fetchStoreError(w, "plugin_fetch_fail_failed", err)
		return
	}
	s.bus.Notify(m.RoomID)
	s.log.Info("plugin_fetch_failed", zap.String("room", room), zap.Int64("fetch", id))
	if code == http.StatusNoContent {
		w.WriteHeader(code)
		return
	}
	httpError(w, code, reason)
}

// readFetched reads a daemon's multipart upload for request f: the plugins to store, or why the
// upload is refused (why), or a server error.
func readFetched(w http.ResponseWriter, r *http.Request, f *store.PluginFetch, maxBytes int64) (ps []store.NewPlugin, why string, err error) {
	// Each tar is checked against maxBytes as it is read; the total bounds the form around them.
	r.Body = http.MaxBytesReader(w, r.Body, int64(store.MaxRoomPlugins)*(maxBytes+int64(plugin.MaxFiles+2)<<13)+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, "the upload is not multipart/form-data", nil
	}
	var commit, refName string
	seen := map[string]bool{}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "the upload is malformed: " + err.Error(), nil
		}
		switch part.FormName() {
		case "commit", "ref_name":
			b, err := io.ReadAll(io.LimitReader(part, 256))
			if err != nil {
				return nil, "the upload is malformed", nil
			}
			if part.FormName() == "commit" {
				commit = string(b)
			} else {
				refName = sanitize(string(b), 200)
			}
		case "snapshot":
			entry := part.FileName()
			if entry == "." {
				entry = ""
			}
			if seen[entry] || len(seen) == store.MaxRoomPlugins {
				return nil, "the upload holds a plugin twice or too many plugins", nil
			}
			seen[entry] = true
			snap, err := plugin.ValidateTar(part, maxBytes)
			if err != nil {
				return nil, err.Error(), nil
			}
			m, err := snap.Manifest()
			if err != nil {
				return nil, err.Error(), nil
			}
			files, _ := json.Marshal(snap.Files) // plain structs: cannot fail
			manifest, _ := json.Marshal(m)       // idem
			ps = append(ps, store.NewPlugin{Name: m.Name, URL: f.URL, Ref: f.Ref, Entry: entry,
				PluginSnapshot: store.PluginSnapshot{SHA256: snap.SHA256, Size: snap.Size, Files: len(snap.Files), FileList: files, Manifest: manifest, Data: snap.Tar}})
		}
		_ = part.Close()
	}
	if !commitRe.MatchString(commit) {
		return nil, "the upload names no commit", nil
	}
	if refName == "" {
		refName = f.Ref
	}
	// The daemon adds what the room asked for, nothing else: the request's entries, or the repo
	// itself when it named none.
	want := f.Entries
	if len(want) == 0 {
		want = []string{""}
	}
	got := make([]string, 0, len(ps))
	for i := range ps {
		ps[i].Commit, ps[i].RefName = commit, refName
		got = append(got, ps[i].Entry)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		return nil, "the AI host sent other plugins than the room asked for", nil
	}
	return ps, "", nil
}
