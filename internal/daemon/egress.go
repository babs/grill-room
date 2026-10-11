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
	r := d.cur
	d.mu.Unlock()
	d.o.Log.Info("allowlist_changed", zap.Strings("allowlist", list))
	if ag := r.agentIfAny(); ag != nil {
		if err := ag.SetAllowlist(ctx, list); err != nil {
			return fmt.Errorf("saved, but the running sandbox kept its old list: %w", err)
		}
	}
	return nil
}

// Denied returns the last n egress requests the current room's sandbox was refused, newest first.
func (d *Daemon) Denied(ctx context.Context, n int) ([]sandbox.Denied, error) {
	d.mu.Lock()
	r := d.cur
	d.mu.Unlock()
	ag := r.agentIfAny()
	if ag == nil {
		return []sandbox.Denied{}, nil
	}
	return ag.Denied(ctx, n)
}

// agentIfAny is the room run's sandbox, nil before its first turn or with no room.
func (r *roomRun) agentIfAny() Agent {
	if r == nil {
		return nil
	}
	r.agentMu.Lock()
	defer r.agentMu.Unlock()
	return r.ag
}
