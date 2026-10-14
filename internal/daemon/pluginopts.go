package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// OptionValues are the values the host gave one room plugin's options. Plugins are keyed by the
// room server's plugin id, so a plugin removed and added again under its name starts with none.
// Name is kept for whoever reads the file; Sensitive names the options the plugin declared
// sensitive when the values were saved, so their values are redacted even once the plugin no
// longer declares them.
type OptionValues struct {
	Name      string         `json:"name"`
	Values    map[string]any `json:"values"`
	Sensitive []string       `json:"sensitive,omitempty"`
}

func (v OptionValues) clone() OptionValues {
	v.Values, v.Sensitive = maps.Clone(v.Values), slices.Clone(v.Sensitive)
	return v
}

// PluginOptions keeps the option values per room link and plugin id, in an owner-only file of the
// instance's state dir (009-AC22). They never leave this machine but for the turn's sandbox: the
// room server, a room and an export get none.
type PluginOptions struct {
	path string
	mu   sync.Mutex
	// m is room link → plugin id → values; loaded once, guarded by mu.
	m map[string]map[int64]OptionValues
}

// NewPluginOptions keeps the values in dir/plugin-options.json; an empty dir keeps them in memory.
func NewPluginOptions(dir string) (*PluginOptions, error) {
	o := &PluginOptions{m: map[string]map[int64]OptionValues{}}
	if dir == "" {
		return o, nil
	}
	o.path = filepath.Join(dir, "plugin-options.json")
	b, err := os.ReadFile(o.path)
	if errors.Is(err, os.ErrNotExist) {
		return o, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read plugin options: %w", err)
	}
	if err := json.Unmarshal(b, &o.m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", o.path, err)
	}
	return o, nil
}

// Get returns a copy of plugin id's values in room.
func (o *PluginOptions) Get(room string, id int64) OptionValues {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.m[room][id].clone()
}

// Room returns a copy of every plugin's values in room.
func (o *PluginOptions) Room(room string) map[int64]OptionValues {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make(map[int64]OptionValues, len(o.m[room]))
	for id, v := range o.m[room] {
		out[id] = v.clone()
	}
	return out
}

// Set replaces plugin id's values in room and saves the file; no value forgets the plugin.
func (o *PluginOptions) Set(room string, id int64, v OptionValues) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	next := o.copyLocked()
	if next[room] == nil {
		next[room] = map[int64]OptionValues{}
	}
	if len(v.Values) == 0 {
		delete(next[room], id)
	} else {
		next[room][id] = v.clone()
	}
	return o.saveLocked(next)
}

// Keep forgets the values of room's plugins whose id is not in ids: a removed plugin's values go.
func (o *PluginOptions) Keep(room string, ids []int64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	next := o.copyLocked()
	changed := false
	for id := range next[room] {
		if !slices.Contains(ids, id) {
			delete(next[room], id)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return o.saveLocked(next)
}

func (o *PluginOptions) copyLocked() map[string]map[int64]OptionValues {
	next := make(map[string]map[int64]OptionValues, len(o.m))
	for r, ps := range o.m {
		next[r] = maps.Clone(ps)
	}
	return next
}

// saveLocked writes next and makes it current; the caller holds mu.
func (o *PluginOptions) saveLocked(next map[string]map[int64]OptionValues) error {
	for r, ps := range next {
		if len(ps) == 0 {
			delete(next, r)
		}
	}
	if o.path != "" {
		b, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("encode plugin options: %w", err)
		}
		// writeFileAtomic: mode 0600, or the owner-only DACL on Windows, as tokens.json.
		if err := writeFileAtomic(o.path, b); err != nil {
			return fmt.Errorf("save plugin options: %w", err)
		}
	}
	o.m = next
	return nil
}
