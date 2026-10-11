package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/turn"
)

// Settings are the host's choices for the turns this daemon runs. They travel with the host: the
// room applies the current host daemon's settings, whoever hosts.
type Settings struct {
	// MaxModel and MaxEffort cap what a persona runs with; empty means no cap (002-AC19).
	MaxModel  string `json:"max_model"`
	MaxEffort string `json:"max_effort"`
	// ApproveAll holds every new persona and persona change until the host approves it (002-AC21).
	ApproveAll bool `json:"approve_all"`
}

// Normalize maps MaxModel onto its family alias, "claude-sonnet-5-5" to "sonnet", and rejects a
// model or effort outside the known orders.
func (s Settings) Normalize() (Settings, error) {
	if s.MaxModel != "" {
		f := turn.ModelFamily(s.MaxModel)
		if f == "" {
			return s, fmt.Errorf("max model %q: want one of %v, or a model id naming one", s.MaxModel, turn.Models)
		}
		s.MaxModel = f
	}
	if s.MaxEffort != "" && !slices.Contains(turn.Efforts, s.MaxEffort) {
		return s, fmt.Errorf("max effort %q: want one of %v", s.MaxEffort, turn.Efforts)
	}
	return s, nil
}

// Limit is the cap as a turn level.
func (s Settings) Limit() turn.Level { return turn.Level{Model: s.MaxModel, Effort: s.MaxEffort} }

// LoadSettings reads the settings the page saved at path; with no file it returns def, the
// environment's.
func LoadSettings(path string, def Settings) (Settings, error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return def.Normalize()
	}
	if err != nil {
		return def, fmt.Errorf("read settings: %w", err)
	}
	var s Settings
	if err := json.Unmarshal(b, &s); err != nil {
		return def, fmt.Errorf("parse %s: %w", path, err)
	}
	return s.Normalize()
}

// SetSettings applies and saves s, then tells the room at once.
func (d *Daemon) SetSettings(s Settings) error {
	s, err := s.Normalize()
	if err != nil {
		return err
	}
	// Held across the file and the status: two saves must leave both with the same one.
	d.settingsMu.Lock()
	defer d.settingsMu.Unlock()
	if d.o.SettingsPath != "" {
		b, err := json.Marshal(s)
		if err != nil {
			return fmt.Errorf("encode settings: %w", err)
		}
		if err := writeFileAtomic(d.o.SettingsPath, b); err != nil {
			return fmt.Errorf("save settings: %w", err)
		}
	}
	d.mu.Lock()
	d.st.Settings = s
	d.mu.Unlock()
	d.o.Log.Info("settings_changed", zap.String("max_model", s.MaxModel), zap.String("max_effort", s.MaxEffort), zap.Bool("approve_all", s.ApproveAll))
	select {
	case d.changed <- struct{}{}:
	default:
	}
	return nil
}

// writeFileAtomic writes b to path, owner-only, through a rename: a crash mid-write keeps the old file.
func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // paths under the supervisor's own state dirs
		return fmt.Errorf("create state dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path) //nolint:gosec // paths under the supervisor's own state dirs
}
