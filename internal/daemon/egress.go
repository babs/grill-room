package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/sandbox"
)

// LoadAllowlist reads the egress allowlist the page saved at path; with no file it is the default.
func LoadAllowlist(path string) ([]string, error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return slices.Clone(sandbox.DefaultAllowlist), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read allowlist: %w", err)
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return sandbox.NormalizeAllowlist(list)
}

// SetAllowlist saves the domains the sandbox may reach and hands them to the running sandbox, if
// any (002-AC36).
func (d *Daemon) SetAllowlist(ctx context.Context, list []string) error {
	list, err := sandbox.NormalizeAllowlist(list)
	if err != nil {
		return err
	}
	d.settingsMu.Lock()
	defer d.settingsMu.Unlock()
	if d.o.AllowlistPath != "" {
		b, err := json.Marshal(list)
		if err != nil {
			return fmt.Errorf("encode allowlist: %w", err)
		}
		if err := writeFileAtomic(d.o.AllowlistPath, b); err != nil {
			return fmt.Errorf("save allowlist: %w", err)
		}
	}
	d.mu.Lock()
	d.st.Allowlist = list
	d.mu.Unlock()
	d.o.Log.Info("allowlist_changed", zap.Strings("allowlist", list))
	var errs []error
	for _, ag := range d.agents() {
		if err := ag.SetAllowlist(ctx, list); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("saved, but a running sandbox kept its old list: %w", err)
	}
	return nil
}

// Egress returns the last n egress requests the rooms' sandboxes were refused, newest first, and
// the requests they let through, by host, summed over the sandboxes (003b-AC16).
func (d *Daemon) Egress(ctx context.Context, n int) (sandbox.Egress, error) {
	out := sandbox.Egress{Denied: []sandbox.Denied{}, Hits: map[string]int{}}
	for _, ag := range d.agents() {
		got, err := ag.Egress(ctx, n)
		if err != nil {
			return sandbox.Egress{}, err
		}
		out.Denied = append(out.Denied, got.Denied...)
		for h, c := range got.Hits {
			out.Hits[h] += c
		}
	}
	slices.SortStableFunc(out.Denied, func(a, b sandbox.Denied) int { return b.Time.Compare(a.Time) })
	if n >= 0 && n < len(out.Denied) {
		out.Denied = out.Denied[:n]
	}
	return out, nil
}

// agents lists the sandboxes of every room, those not created yet left out.
func (d *Daemon) agents() []Agent {
	var out []Agent
	for _, r := range d.runs() {
		r.agentMu.Lock()
		for _, sl := range r.sandboxes {
			if sl.ag != nil {
				out = append(out, sl.ag)
			}
		}
		r.agentMu.Unlock()
	}
	return out
}
