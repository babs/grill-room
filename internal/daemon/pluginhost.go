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
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/plugin"
	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/turn"
)

// HostPlugin is a room plugin as the host's page shows it (009-AC22, 009-AC24, 009-AC25).
type HostPlugin struct {
	// ID is the room's plugin id: the page saves the options under it.
	ID      int64           `json:"id"`
	Name    string          `json:"name"`
	Version int             `json:"version"`
	Options []plugin.Option `json:"options,omitempty"`
	// Values holds the values of the options that are not sensitive; Set names every option that
	// holds a value. A sensitive value leaves the options file for the sandbox only.
	Values map[string]any `json:"values,omitempty"`
	Set    []string       `json:"set,omitempty"`
	// NeedsSetup is set while a required option with no default has no value: the plugin does not
	// load.
	NeedsSetup bool     `json:"needs_setup,omitempty"`
	Domains    []string `json:"domains,omitempty"`
	// OffAllowlist are the Domains the egress allowlist does not let the sandbox reach.
	OffAllowlist []string `json:"off_allowlist,omitempty"`
}

// HostFetch is a room's request that this host fetch a repo the server cannot read (009-AC19).
type HostFetch struct {
	ID          int64     `json:"id"`
	PluginName  string    `json:"plugin_name,omitempty"`
	URL         string    `json:"url"`
	Ref         string    `json:"ref"`
	Entries     []string  `json:"entries,omitempty"`
	RequestedBy string    `json:"requested_by"`
	CreatedAt   time.Time `json:"created_at"`
	// Fetching is set while this daemon fetches it.
	Fetching bool `json:"fetching,omitempty"`
}

// roomPlugins is what the room server lists for its host's daemon (GET .../daemon/plugins).
type roomPlugins struct {
	Plugins []struct {
		ID      int64           `json:"id"`
		Name    string          `json:"name"`
		Version int             `json:"version"`
		Options []plugin.Option `json:"options"`
		Domains []string        `json:"domains"`
	} `json:"plugins"`
	Fetches  []HostFetch `json:"fetches"`
	MaxBytes int64       `json:"max_bytes"`
}

// maxRoomPluginsList bounds the room's answer: ten plugins and ten requests.
const maxRoomPluginsList = 1 << 20

// needsSetup reports whether a required option with no default has no value (009-AC24).
func needsSetup(opts []turn.PluginOption, values map[string]any) bool {
	return slices.ContainsFunc(opts, func(o turn.PluginOption) bool { return o.Required && o.Default == nil && values[o.Key] == nil })
}

func turnOptions(opts []plugin.Option) []turn.PluginOption {
	out := make([]turn.PluginOption, len(opts))
	for i, o := range opts {
		out[i] = turn.PluginOption{Key: o.Key, Sensitive: o.Sensitive, Required: o.Required, Default: o.Default}
	}
	return out
}

// hostPluginsLocked describes the room's plugins for the page; the caller holds d.mu.
func (d *Daemon) hostPluginsLocked(r *roomRun) []HostPlugin {
	if r.plugins == nil {
		return nil
	}
	values := d.o.PluginOptions.Room(r.room.String())
	out := make([]HostPlugin, 0, len(r.plugins.Plugins))
	for _, p := range r.plugins.Plugins {
		hp := HostPlugin{ID: p.ID, Name: p.Name, Version: p.Version, Options: p.Options, Domains: p.Domains}
		vs := values[p.ID].Values
		for _, o := range p.Options {
			v, ok := vs[o.Key]
			if !ok {
				continue
			}
			hp.Set = append(hp.Set, o.Key)
			if !o.Sensitive {
				if hp.Values == nil {
					hp.Values = map[string]any{}
				}
				hp.Values[o.Key] = v
			}
		}
		hp.NeedsSetup = needsSetup(turnOptions(p.Options), vs)
		policy := d.policyLocked(r.room.String())
		for _, dom := range p.Domains {
			if !covered(policy, dom) {
				hp.OffAllowlist = append(hp.OffAllowlist, dom)
			}
		}
		out = append(out, hp)
	}
	return out
}

// hostFetchesLocked lists the room's requests waiting for this host; the caller holds d.mu.
func hostFetchesLocked(r *roomRun) []HostFetch {
	if r.plugins == nil {
		return nil
	}
	out := slices.Clone(r.plugins.Fetches)
	for i := range out {
		out[i].Fetching = r.fetching[out[i].ID]
	}
	return out
}

// loadPlugins reads the room's plugins and the requests waiting for this host, for the page. The
// room sends a "plugins" event whenever they change. A room server from before spec 009 Phase 3
// answers 404: the page shows nothing.
func (r *roomRun) loadPlugins(ctx context.Context, tok string) {
	r.pluginsMu.Lock()
	defer r.pluginsMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := r.get(ctx, tok, "plugins")
	if err != nil {
		r.d.o.Log.Warn("room_plugins_failed", zap.String("room", r.room.String()), zap.Error(err))
		return
	}
	defer resp.Body.Close()
	var rp roomPlugins
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRoomPluginsList)).Decode(&rp); err != nil {
		r.d.o.Log.Warn("room_plugins_failed", zap.String("room", r.room.String()), zap.Error(err))
		return
	}
	r.d.mu.Lock()
	r.plugins = &rp
	r.d.mu.Unlock()
	ids := make([]int64, len(rp.Plugins))
	for i, p := range rp.Plugins {
		ids[i] = p.ID
	}
	// A removed plugin's values go: one added later under its name starts with none (009-AC22).
	if err := r.d.o.PluginOptions.Keep(r.room.String(), ids); err != nil {
		r.d.o.Log.Warn("plugin_options_prune_failed", zap.String("room", r.room.String()), zap.Error(err))
	}
}

// SetPluginOptions saves the values the host gave the options of plugin id in room (009-AC22): an absent
// key keeps its value, an empty string or a key in clear forgets it. Only the plugin's declared
// options are taken, each of its type. A value is never logged.
func (d *Daemon) SetPluginOptions(_ context.Context, room string, id int64, values map[string]any, clear []string) error {
	room = roomKey(room)
	d.mu.Lock()
	r := d.rooms[room]
	var opts []plugin.Option
	var name string
	found := false
	if r != nil && r.plugins != nil {
		for _, p := range r.plugins.Plugins {
			if p.ID == id {
				opts, name, found = p.Options, p.Name, true
			}
		}
	}
	d.mu.Unlock()
	if !found {
		return errors.New("this room has no such plugin")
	}
	cur := d.o.PluginOptions.Get(room, id)
	next := OptionValues{Name: name, Values: map[string]any{}, Sensitive: cur.Sensitive}
	for k := range values {
		if !slices.ContainsFunc(opts, func(o plugin.Option) bool { return o.Key == k }) {
			return fmt.Errorf("the plugin has no option %q", k)
		}
	}
	for _, o := range opts {
		v, given := values[o.Key]
		switch {
		case slices.Contains(clear, o.Key) || given && v == "":
			continue
		case !given:
			if old, ok := cur.Values[o.Key]; ok {
				next.Values[o.Key] = old
			}
		default:
			if err := checkOption(o, v); err != nil {
				return err
			}
			next.Values[o.Key] = v
		}
		if o.Sensitive && !slices.Contains(next.Sensitive, o.Key) {
			next.Sensitive = append(next.Sensitive, o.Key)
		}
	}
	slices.Sort(next.Sensitive)
	if err := d.o.PluginOptions.Set(room, id, next); err != nil {
		return err
	}
	d.o.Log.Info("plugin_options_saved", zap.String("room", room), zap.String("plugin", name), zap.Int("set", len(next.Values)))
	return nil
}

// maxOptionLen bounds an option's value.
const maxOptionLen = 4096

// checkOption checks v against o's type; the error names the option, never the value.
func checkOption(o plugin.Option, v any) error {
	var ok bool
	want := "text of at most 4096 bytes"
	switch o.Type {
	case "boolean":
		_, ok = v.(bool)
		want = "yes or no"
	case "number":
		_, ok = v.(float64)
		want = "a number"
	default:
		s, isString := v.(string)
		ok = isString && len(s) <= maxOptionLen && !strings.ContainsRune(s, 0)
	}
	if !ok {
		return fmt.Errorf("the option %s takes %s", o.Key, want)
	}
	return nil
}

// errNoRequest is the answer to a press on a request the room no longer lists.
var errNoRequest = errors.New("no such request: it was done or cancelled in the room")

// PluginFetch answers a room's request that this host fetch a repo (009-AC19): Fetch starts it in
// the background and the page follows it; Decline tells the room at once. Nothing touches git
// before the host's press.
func (d *Daemon) PluginFetch(ctx context.Context, room string, id int64, accept bool) error {
	room = roomKey(room)
	d.mu.Lock()
	r := d.rooms[room]
	var f *HostFetch
	if r != nil && r.plugins != nil {
		if i := slices.IndexFunc(r.plugins.Fetches, func(x HostFetch) bool { return x.ID == id }); i >= 0 {
			f = &r.plugins.Fetches[i]
		}
	}
	busy := r != nil && r.fetching[id]
	if f != nil && accept && !busy {
		if r.fetching == nil {
			r.fetching = map[int64]bool{}
		}
		r.fetching[id] = true
	}
	var maxBytes int64
	var fc HostFetch
	if f != nil {
		fc, maxBytes = *f, r.plugins.MaxBytes
	}
	d.mu.Unlock()
	switch {
	case f == nil:
		return errNoRequest
	case busy:
		return errors.New("this request is being fetched already")
	}
	tok, err := d.o.Tokens.Get(r.room)
	if err == nil && tok == "" {
		err = errors.New("this daemon is not paired with the room")
	}
	if err != nil {
		if accept {
			r.doneFetching(id)
		}
		return err
	}
	if !accept {
		d.o.Log.Info("plugin_fetch_declined", zap.String("room", room), zap.Int64("fetch", id))
		return r.postFetchFailure(ctx, tok, id, fetchFailure{Declined: true})
	}
	d.o.Log.Info("plugin_fetch_started", zap.String("room", room), zap.Int64("fetch", id))
	d.loops.Go(func() {
		defer r.doneFetching(id)
		r.fetchForRoom(r.ctx, tok, fc, maxBytes)
	})
	return nil
}

// roomKey is the link a room run is kept under, for a link as the page sends it.
func roomKey(link string) string {
	if r, err := ParseRoomURL(link); err == nil {
		return r.String()
	}
	return link
}

func (r *roomRun) doneFetching(id int64) {
	r.d.mu.Lock()
	delete(r.fetching, id)
	r.d.mu.Unlock()
}

// fetchFailure is what the daemon posts when it did not fetch (POST .../daemon/plugin-fetches/{id}).
type fetchFailure struct {
	Error    string `json:"error,omitempty"`
	Declined bool   `json:"declined,omitempty"`
}

func (r *roomRun) fetchURL(id int64) string {
	return r.daemonURL("plugin-fetches/" + strconv.FormatInt(id, 10))
}

func (r *roomRun) postFetchFailure(ctx context.Context, tok string, id int64, f fetchFailure) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := r.d.roomRequest(ctx, http.MethodPost, r.fetchURL(id), tok, f)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return apiError(resp)
	}
	return nil
}

// turnPluginsSetup splits the job's plugins into those the turn loads and those it leaves out for a
// required option the host has not set (009-AC24), and gathers the loaded ones' values as Claude
// Code reads them (009-AC23). redact holds the room's sensitive values, whichever plugin they
// belong to, for the redaction of what the turn posts: the string values of the options a plugin
// declared sensitive when the host saved them, or declares so in this job.
func (d *Daemon) turnPluginsSetup(room string, ps []turn.Plugin) (load []turn.Plugin, skipped []turn.SkippedPlugin, opts sandbox.PluginOptions, redact []string) {
	all := d.o.PluginOptions.Room(room)
	opts = sandbox.PluginOptions{Secrets: map[string]map[string]any{}, Configs: map[string]map[string]any{}}
	sensitive := map[int64][]string{}
	for _, p := range ps {
		values := all[p.ID].Values
		for _, o := range p.Options {
			if o.Sensitive {
				sensitive[p.ID] = append(sensitive[p.ID], o.Key)
			}
		}
		if needsSetup(p.Options, values) {
			skipped = append(skipped, turn.SkippedPlugin{Name: p.Name, Reason: turn.NeedsHostSetup})
			continue
		}
		load = append(load, p)
		id := p.Name + "@inline"
		for _, o := range p.Options {
			v, ok := values[o.Key]
			if !ok {
				v = o.Default
			}
			if v == nil {
				continue
			}
			into := opts.Configs
			if o.Sensitive {
				into = opts.Secrets
			}
			if into[id] == nil {
				into[id] = map[string]any{}
			}
			into[id][o.Key] = v
		}
	}
	for id, v := range all {
		for k, val := range v.Values {
			s, isString := val.(string)
			if isString && len(s) >= minRedact && (slices.Contains(v.Sensitive, k) || slices.Contains(sensitive[id], k)) {
				redact = append(redact, s)
			}
		}
	}
	slices.Sort(redact)
	return load, skipped, opts, slices.Compact(redact)
}

// minRedact is the shortest value redacted: a shorter one would blank common words.
const minRedact = 4
