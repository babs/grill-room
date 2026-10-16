package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/sandbox"
)

// EgressState is a server's egress policy as the page and the rooms set it, and as allowlist.json
// holds it (014): the two "Allow every website" switches, the entries for every room of the server,
// and each room's own entries by room link.
type EgressState struct {
	AllWebsites  bool                    `json:"all_websites"`
	LocalNetwork bool                    `json:"local_network"`
	Server       []AllowEntry            `json:"server"`
	Rooms        map[string][]AllowEntry `json:"rooms,omitempty"`
}

// AllowEntry is one domain of a list; By and At say who added it from a room's chat, and when
// (014-AC15). Entries the page adds carry neither.
type AllowEntry struct {
	Domain string     `json:"domain"`
	By     string     `json:"by,omitempty"`
	At     *time.Time `json:"at,omitempty"`
}

// EgressSwitches are the server's "Allow every website" switch and its local-network extension.
type EgressSwitches struct {
	AllWebsites  bool `json:"all_websites"`
	LocalNetwork bool `json:"local_network"`
}

func domains(es []AllowEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Domain
	}
	return out
}

// normalizeEntries checks and normalizes the domains of es as NormalizeAllowlist does, keeping each
// surviving domain's By and At.
func normalizeEntries(es []AllowEntry) ([]AllowEntry, error) {
	list, err := sandbox.NormalizeAllowlist(domains(es))
	if err != nil {
		return nil, err
	}
	return entriesFor(list, es), nil
}

// entriesFor returns list as entries, each with the By and At an entry of old had for its domain.
func entriesFor(list []string, old []AllowEntry) []AllowEntry {
	out := make([]AllowEntry, len(list))
	for i, d := range list {
		out[i] = AllowEntry{Domain: d}
		if j := slices.IndexFunc(old, func(e AllowEntry) bool { return e.Domain == d }); j >= 0 {
			out[i].By, out[i].At = old[j].By, old[j].At
		}
	}
	return out
}

// LoadEgress reads the egress policy the page saved at path; with no file it is the default list
// and both switches off. A file from before 014 holds a bare list: it becomes the server's entries.
func LoadEgress(path string) (EgressState, error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return EgressState{Server: entriesFor(sandbox.DefaultAllowlist, nil)}, nil
	}
	if err != nil {
		return EgressState{}, fmt.Errorf("read allowlist: %w", err)
	}
	var st EgressState
	var legacy []string
	if json.Unmarshal(b, &legacy) == nil {
		st.Server = entriesFor(legacy, nil)
	} else if err := json.Unmarshal(b, &st); err != nil {
		return EgressState{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if st.Server, err = normalizeEntries(st.Server); err != nil {
		return EgressState{}, fmt.Errorf("%s: %w", path, err)
	}
	for room, es := range st.Rooms {
		if st.Rooms[room], err = normalizeEntries(es); err != nil {
			return EgressState{}, fmt.Errorf("%s, room %s: %w", path, room, err)
		}
	}
	st.LocalNetwork = st.LocalNetwork && st.AllWebsites
	return st, nil
}

// policyLocked is what room's sandboxes may reach: the server's list and the room's own, under
// the server's switches; the caller holds d.mu.
func (d *Daemon) policyLocked(room string) sandbox.Policy {
	e := d.st.Egress
	return sandbox.Policy{Allow: append(domains(e.Server), domains(e.Rooms[room])...), AllWebsites: e.AllWebsites, LocalNetwork: e.LocalNetwork}
}

func (d *Daemon) policy(room string) sandbox.Policy {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.policyLocked(room)
}

// setEgress saves next and hands every running sandbox its room's policy (002-AC36). The caller
// holds settingsMu, so that two changes never interleave their saves.
func (d *Daemon) setEgress(ctx context.Context, next EgressState) error {
	if d.o.AllowlistPath != "" {
		b, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("encode allowlist: %w", err)
		}
		if err := writeFileAtomic(d.o.AllowlistPath, b); err != nil {
			return fmt.Errorf("save allowlist: %w", err)
		}
	}
	d.mu.Lock()
	d.st.Egress, d.st.Allowlist = next, domains(next.Server)
	d.mu.Unlock()
	d.o.Log.Info("allowlist_changed", zap.Strings("allowlist", domains(next.Server)), zap.Bool("all_websites", next.AllWebsites),
		zap.Bool("local_network", next.LocalNetwork))
	var errs []error
	for _, r := range d.runs() {
		// Under agentMu, as agent() reads the policy for a sandbox it builds.
		r.agentMu.Lock()
		p := d.policy(r.room.String())
		for _, sl := range r.sandboxes {
			if sl.ag != nil {
				if err := sl.ag.SetPolicy(ctx, p); err != nil {
					errs = append(errs, err)
				}
			}
		}
		r.agentMu.Unlock()
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("saved, but a running sandbox kept its old list: %w", err)
	}
	return nil
}

// SetAllowlist saves the domains every room's sandboxes may reach and hands them to the running
// ones (002-AC36). An entry added from a room keeps who added it while it stays on the list; its
// removal is told to the connected rooms (014-AC15).
func (d *Daemon) SetAllowlist(ctx context.Context, list []string) error {
	list, err := sandbox.NormalizeAllowlist(list)
	if err != nil {
		return err
	}
	d.settingsMu.Lock()
	prev := d.Status().Egress.Server
	next := d.Status().Egress
	next.Server = entriesFor(list, next.Server)
	err = d.setEgress(ctx, next)
	d.settingsMu.Unlock()
	var gone []string
	for _, e := range prev {
		if e.By != "" && !slices.Contains(list, e.Domain) {
			gone = append(gone, e.Domain)
		}
	}
	if len(gone) > 0 {
		// The page's answer does not wait for the rooms: each may take its timeout.
		d.loops.Go(func() {
			for _, dom := range gone {
				d.tellRevoked(context.WithoutCancel(ctx), "", dom, "server")
			}
		})
	}
	return err
}

// SetEgressSwitches saves the server's "Allow every website" switch and its local-network
// extension, which needs the first; the running sandboxes take them at once (014-AC1, 014-AC26).
func (d *Daemon) SetEgressSwitches(ctx context.Context, sw EgressSwitches) error {
	d.settingsMu.Lock()
	defer d.settingsMu.Unlock()
	next := d.Status().Egress
	next.AllWebsites, next.LocalNetwork = sw.AllWebsites, sw.LocalNetwork && sw.AllWebsites
	return d.setEgress(ctx, next)
}

// Egress returns the last n egress requests the rooms' sandboxes were refused, newest first, and
// the requests they let through, by host, summed over the sandboxes and by room (003b-AC16,
// 014-AC3).
func (d *Daemon) Egress(ctx context.Context, n int) (sandbox.Egress, error) {
	out := sandbox.Egress{Denied: []sandbox.Denied{}, Hits: map[string]int{}, RoomHits: map[string]map[string]int{}}
	for _, r := range d.runs() {
		r.agentMu.Lock()
		var ags []Agent
		for _, sl := range r.sandboxes {
			if sl.ag != nil {
				ags = append(ags, sl.ag)
			}
		}
		r.agentMu.Unlock()
		for _, ag := range ags {
			got, err := ag.Egress(ctx, n)
			if err != nil {
				return sandbox.Egress{}, err
			}
			out.Denied = append(out.Denied, got.Denied...)
			room := r.room.String()
			for h, c := range got.Hits {
				out.Hits[h] += c
				if out.RoomHits[room] == nil {
					out.RoomHits[room] = map[string]int{}
				}
				out.RoomHits[room][h] += c
			}
		}
	}
	slices.SortStableFunc(out.Denied, func(a, b sandbox.Denied) int { return b.Time.Compare(a.Time) })
	if n >= 0 && n < len(out.Denied) {
		out.Denied = out.Denied[:n]
	}
	return out, nil
}

// logEgress writes one egress line per request the sandbox's proxy saw during a turn of persona in
// room; the worker's logger adds the server (014-AC3).

func (d *Daemon) logEgress(room, persona string, reqs []sandbox.EgressRequest) {
	for _, q := range reqs {
		verdict := "allowed"
		if !q.Allowed {
			verdict = "denied"
		}
		d.o.Log.Info("egress", zap.String("room", room), zap.String("persona", persona), zap.String("host", q.Host),
			zap.String("method", q.Method), zap.String("verdict", verdict))
	}
}

// covered reports whether p lets the sandbox reach domain (014-AC17).
func covered(p sandbox.Policy, domain string) bool {
	return p.AllWebsites || sandbox.Allowed(p.Allow, domain)
}

// blockedDomains are the domains reqs were refused a CONNECT to port 443 of, once each, in order: an
// IP, an Anthropic host or another port stays on the daemon page only (014-AC8).
func blockedDomains(reqs []sandbox.EgressRequest) []string {
	var out []string
	for _, q := range reqs {
		if q.Allowed || q.Method != "CONNECT" || strings.Contains(q.Host, ":") || slices.Contains(out, q.Host) {
			continue
		}
		if _, err := sandbox.NormalizeAllowlist([]string{q.Host}); err == nil {
			out = append(out, q.Host)
		}
	}
	return out
}

// AllowEgress adds domain to room's own list, scope "room", or to every room's, scope "server", as
// by approved it in the room; the running sandboxes take it at once (014-AC10, 014-AC11, 014-AC16).
func (d *Daemon) AllowEgress(ctx context.Context, room, domain, scope, by string) error {
	list, err := sandbox.NormalizeAllowlist([]string{domain})
	if err != nil {
		return err
	}
	if len(list) != 1 || strings.HasPrefix(list[0], ".") {
		return fmt.Errorf("%q: one domain name, without a leading dot", domain)
	}
	at := time.Now().UTC()
	add := AllowEntry{Domain: list[0], By: by, At: &at}
	d.settingsMu.Lock()
	defer d.settingsMu.Unlock()
	next := d.Status().Egress
	switch scope {
	case "room":
		es := next.Rooms[room]
		if slices.ContainsFunc(es, func(e AllowEntry) bool { return e.Domain == add.Domain }) {
			return nil
		}
		if es, err = normalizeEntries(append(slices.Clone(es), add)); err != nil {
			return fmt.Errorf("this room's websites: %w", err)
		}
		next.Rooms = maps.Clone(next.Rooms)
		if next.Rooms == nil {
			next.Rooms = map[string][]AllowEntry{}
		}
		next.Rooms[room] = es
	case "server":
		if slices.ContainsFunc(next.Server, func(e AllowEntry) bool { return e.Domain == add.Domain }) {
			return nil
		}
		if next.Server, err = normalizeEntries(append(slices.Clone(next.Server), add)); err != nil {
			return fmt.Errorf("every room's websites: %w", err)
		}
	default:
		return fmt.Errorf("scope %q: room or server", scope)
	}
	return d.setEgress(ctx, next)
}

// RemoveRoomEgress takes domain off room's own list; the running sandboxes lose it at once and the
// room, when connected, hears of it (014-AC15).
func (d *Daemon) RemoveRoomEgress(ctx context.Context, room, domain string) error {
	d.settingsMu.Lock()
	next := d.Status().Egress
	es := slices.DeleteFunc(slices.Clone(next.Rooms[room]), func(e AllowEntry) bool { return e.Domain == domain })
	if len(es) == len(next.Rooms[room]) {
		d.settingsMu.Unlock()
		return fmt.Errorf("%s is not on this room's websites", domain)
	}
	next.Rooms = maps.Clone(next.Rooms)
	if len(es) == 0 {
		delete(next.Rooms, room)
	} else {
		next.Rooms[room] = es
	}
	err := d.setEgress(ctx, next)
	d.settingsMu.Unlock()
	if err != nil {
		return err
	}
	d.tellRevoked(ctx, room, domain, "room")
	return nil
}

// forgetRoomEgress drops a removed room's own list (014-AC16).
func (d *Daemon) forgetRoomEgress(ctx context.Context, room string) error {
	d.settingsMu.Lock()
	defer d.settingsMu.Unlock()
	next := d.Status().Egress
	if _, ok := next.Rooms[room]; !ok {
		return nil
	}
	next.Rooms = maps.Clone(next.Rooms)
	delete(next.Rooms, room)
	return d.setEgress(ctx, next)
}

// tellRevoked posts to room, or to every connected room when room is "", that the host removed
// domain from scope's list; a room not connected only gets the daemon's log line (014-AC15).
func (d *Daemon) tellRevoked(ctx context.Context, room, domain, scope string) {
	d.o.Log.Info("egress_revoked", zap.String("room", room), zap.String("domain", domain), zap.String("scope", scope))
	for _, r := range d.runs() {
		if room != "" && r.room.String() != room {
			continue
		}
		d.mu.Lock()
		connected := r.st.Connection == "connected"
		d.mu.Unlock()
		if !connected {
			continue
		}
		tok, err := d.o.Tokens.Get(r.room)
		if err != nil || tok == "" {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		resp, err := d.roomRequest(rctx, http.MethodPost, r.daemonURL("egress-revoked"), tok, map[string]string{"domain": domain, "scope": scope})
		if err == nil {
			if resp.StatusCode/100 != 2 {
				err = apiError(resp)
			}
			_ = resp.Body.Close()
		}
		cancel()
		if err != nil {
			d.o.Log.Warn("egress_revoked_post_failed", zap.String("room", r.room.String()), zap.Error(err))
		}
	}
}

// applyAllow applies a website approval the room handed this daemon, and tells the room whether
// it did (014-AC10).
func (r *roomRun) applyAllow(ctx context.Context, tok string, id int64, domain, scope, by string) {
	err := r.d.AllowEgress(ctx, r.room.String(), domain, scope, by)
	r.d.o.Log.Info("egress_allowed", zap.String("room", r.room.String()), zap.String("domain", domain), zap.String("scope", scope),
		zap.String("by", by), zap.Error(err))
	body := map[string]string{"error": ""}
	if err != nil {
		body["error"] = err.Error()
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := r.d.roomRequest(rctx, http.MethodPost, r.daemonURL("blocked/"+strconv.FormatInt(id, 10)), tok, body)
	if err == nil {
		if resp.StatusCode/100 != 2 {
			err = apiError(resp)
		}
		_ = resp.Body.Close()
	}
	if err != nil {
		r.d.o.Log.Warn("egress_allow_confirm_failed", zap.String("room", r.room.String()), zap.Int64("message", id), zap.Error(err))
	}
}
