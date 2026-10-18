package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Plugin version states (009-AC11, 009-AC13): inspecting until the host's daemon checked it, then
// pending (the host approves everything), approved, or invalid (claude plugin validate failed); the
// host may refuse a pending one.
const (
	PluginInspecting = "inspecting"
	PluginPending    = "pending"
	PluginApproved   = "approved"
	PluginRefused    = "refused"
	PluginInvalid    = "invalid"
)

// Plugin limits (009-AC10).
const (
	MaxRoomPlugins    = 10
	MaxPersonaPlugins = 5
)

var (
	// ErrPluginName is returned when the room has a plugin of that name already.
	ErrPluginName = errors.New("the room has a plugin with that name already")
	// ErrPersonaPluginName is returned when a persona has another active plugin of that name.
	ErrPersonaPluginName = errors.New("the persona already has an active plugin with that name")
	// ErrUpToDate is returned when an update resolves to the plugin's latest commit.
	ErrUpToDate = errors.New("up to date")
	// ErrPluginState is returned when a version is not in the state the action needs.
	ErrPluginState = errors.New("this version is not waiting for that")
)

// purgePlugins drops the removed plugins no running turn loads (009-AC12).
const purgePlugins = `DELETE FROM room_plugins WHERE removed_at IS NOT NULL AND id NOT IN (
	SELECT v.plugin_id FROM turns t CROSS JOIN jsonb_array_elements(t.plugins::jsonb) j(value)
	JOIN plugin_versions v ON v.id = (j.value->>'version_id')::bigint
	WHERE t.status = 'running')`

// dropSuperseded deletes the snapshots of versions older than their plugin's running one that no
// running turn loads: no turn will load them again, and the room's quota counts their bytes. The
// version rows stay for the history; a change summary needs the running version's file list only.
const dropSuperseded = `DELETE FROM plugin_snapshots WHERE version_id IN (SELECT v.id FROM plugin_versions v WHERE v.state != 'inspecting'
	AND v.n < (SELECT max(w.n) FROM plugin_versions w WHERE w.plugin_id = v.plugin_id AND w.state = 'approved'))
	AND version_id NOT IN (SELECT (j.value->>'version_id')::bigint FROM turns t, jsonb_array_elements(t.plugins::jsonb) j(value)
	WHERE t.status = 'running')`

// PluginVersion is one snapshot of a room plugin. Manifest is what the server read from its files;
// Report is what the host's daemon found when it inspected it. FileList is the snapshot's
// [{path, sha256, size}].
type PluginVersion struct {
	ID        int64           `json:"id"`
	N         int             `json:"n"`
	Commit    string          `json:"commit"`
	SHA256    string          `json:"sha256"`
	Size      int64           `json:"size"`
	Files     int             `json:"files"`
	FileList  json.RawMessage `json:"-"`
	Manifest  json.RawMessage `json:"manifest"`
	Report    json.RawMessage `json:"report,omitempty"`
	State     string          `json:"state"`
	Reason    string          `json:"reason,omitempty"`
	AddedBy   string          `json:"added_by"`
	AddedAt   time.Time       `json:"added_at"`
	DecidedBy string          `json:"decided_by,omitempty"`
	DecidedAt *time.Time      `json:"decided_at,omitempty"`
	// Imported tells that its files came from an archive, not from the repo: its commit is the
	// archive's word (009-AC27).
	Imported bool `json:"imported,omitempty"`
}

// RoomPlugin is a plugin of a room with its versions, oldest first, and the personas it is active
// for.
type RoomPlugin struct {
	ID int64 `json:"id"`
	// Name is the plugin's name, from its plugin.json.
	Name string `json:"name"`
	URL  string `json:"url"`
	// Ref is what the admin typed, "" for the default branch; RefName is what the room shows.
	Ref     string `json:"ref"`
	RefName string `json:"ref_name"`
	Entry   string `json:"entry,omitempty"`
	Private bool   `json:"private"`
	// NeedsSetup is set when the last turn that pinned it left it out for an option the AI host has
	// not set (009-AC24).
	NeedsSetup bool            `json:"needs_setup,omitempty"`
	CreatedBy  string          `json:"created_by"`
	CreatedAt  time.Time       `json:"created_at"`
	Versions   []PluginVersion `json:"versions"`
	Personas   []int64         `json:"personas"`
}

// Running is the version turns load: the latest approved one, nil when none is.
func (p *RoomPlugin) Running() *PluginVersion {
	for i := len(p.Versions) - 1; i >= 0; i-- {
		if p.Versions[i].State == PluginApproved {
			return &p.Versions[i]
		}
	}
	return nil
}

// Latest is the newest version.
func (p *RoomPlugin) Latest() *PluginVersion {
	if len(p.Versions) == 0 {
		return nil
	}
	return &p.Versions[len(p.Versions)-1]
}

// PluginSnapshot is a fetched version to store.
type PluginSnapshot struct {
	Commit   string
	SHA256   string
	Size     int64
	Files    int
	FileList json.RawMessage
	Manifest json.RawMessage
	Data     []byte
}

// NewPlugin is a plugin to add, at its first version. Private ones came through the AI host
// (009-AC20).
type NewPlugin struct {
	Name, URL, Ref, RefName, Entry string
	Private                        bool
	PluginSnapshot
}

func shortCommit(c string) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

// pluginsFit returns ErrRoomFull when adding bytes would take the room past its quota (009-AC9).
func pluginsFit(ctx context.Context, tx *txn, roomID string, bytes, defaultQuota int64) error {
	used, quota, err := roomUsage(ctx, tx, roomID, defaultQuota)
	if err != nil {
		return err
	}
	if used+bytes > quota {
		return fmt.Errorf("%w: the plugin's %s would take the room past its %s", ErrRoomFull, mb(bytes), mb(quota))
	}
	return nil
}

// AddPlugins adds plugins to room at their version 1, waiting for the host's daemon to inspect
// them, active for personas, in one transaction: a refusal adds none. Each gets a line in the
// chat (009-AC5, 009-AC10, 009-AC11). ErrNotFound for a persona not in the room, ErrLimit,
// ErrPluginName, ErrPersonaPluginName, ErrRoomFull.
func (s *Store) AddPlugins(ctx context.Context, roomID string, ps []NewPlugin, personas []int64, by *Participant, defaultQuota int64) ([]*Message, error) {
	var msgs []*Message
	err := s.withTx(ctx, "add plugins", func(tx *txn) (err error) {
		msgs, err = addPlugins(ctx, tx, roomID, ps, personas, by, defaultQuota)
		return err
	})
	return msgs, err
}

func addPlugins(ctx context.Context, tx *txn, roomID string, ps []NewPlugin, personas []int64, by *Participant, defaultQuota int64) ([]*Message, error) {
	var msgs []*Message
	err := func() error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM room_plugins WHERE room_id = ? AND removed_at IS NULL`, roomID).Scan(&n); err != nil {
			return fmt.Errorf("add plugins: %w", err)
		}
		if n+len(ps) > MaxRoomPlugins {
			return fmt.Errorf("%w: at most %d plugins per room", ErrLimit, MaxRoomPlugins)
		}
		var total int64
		for _, p := range ps {
			total += int64(len(p.Data))
		}
		if err := pluginsFit(ctx, tx, roomID, total, defaultQuota); err != nil {
			return err
		}
		for _, pid := range personas {
			if err := personaInRoom(ctx, tx, roomID, pid); err != nil {
				return err
			}
		}
		for _, p := range ps {
			var id int64
			err := tx.QueryRowContext(ctx, `INSERT INTO room_plugins(room_id, name, source_url, ref, ref_name, entry, private, created_by_name, created_at)
				VALUES(?,?,?,?,?,?,?,?,?) RETURNING id`, roomID, p.Name, p.URL, p.Ref, p.RefName, p.Entry, p.Private, by.Name, now()).Scan(&id)
			if isUnique(err) {
				return fmt.Errorf("%w: %s", ErrPluginName, p.Name)
			}
			if err != nil {
				return fmt.Errorf("add plugins: %w", err)
			}
			if err := insertPluginVersion(ctx, tx, id, 1, p.PluginSnapshot, by); err != nil {
				return err
			}
			for _, pid := range personas {
				if err := activatePlugin(ctx, tx, pid, id, p.Name); err != nil {
					return err
				}
			}
			m, err := memberLine(ctx, tx, roomID, by, fmt.Sprintf("added the plugin %s, %s @ %s.", p.Name, p.RefName, shortCommit(p.Commit)))
			if err != nil {
				return err
			}
			msgs = append(msgs, m)
		}
		return nil
	}()
	return msgs, err
}

func insertPluginVersion(ctx context.Context, tx *txn, pluginID int64, n int, v PluginSnapshot, by *Participant) error {
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO plugin_versions(plugin_id, n, commit_sha, sha256, size, files, file_list, manifest, state,
		added_by_name, added_by_id, added_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`,
		pluginID, n, v.Commit, v.SHA256, v.Size, v.Files, string(v.FileList), string(v.Manifest), PluginInspecting, by.Name, nullID(by.ID), now()).Scan(&id); err != nil {
		return fmt.Errorf("insert plugin version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_snapshots(version_id, data) VALUES(?, ?)`, id, v.Data); err != nil {
		return fmt.Errorf("insert plugin snapshot: %w", err)
	}
	return nil
}

func personaInRoom(ctx context.Context, tx *txn, roomID string, id int64) error {
	var ok bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM room_personas WHERE id = ? AND room_id = ?)`, id, roomID).Scan(&ok); err != nil {
		return fmt.Errorf("persona: %w", err)
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// activatePlugin turns plugin pluginID, named name, on for persona personaID within the limits.
func activatePlugin(ctx context.Context, tx *txn, personaID, pluginID int64, name string) error {
	var on, clash bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM room_persona_plugins WHERE persona_id = ? AND plugin_id = ?),
		EXISTS (SELECT 1 FROM room_persona_plugins k JOIN room_plugins p ON p.id = k.plugin_id WHERE k.persona_id = ? AND p.name = ? AND p.id != ?)`,
		personaID, pluginID, personaID, name, pluginID).Scan(&on, &clash)
	switch {
	case err != nil:
		return fmt.Errorf("activate plugin: %w", err)
	case on:
		return nil
	case clash:
		return ErrPersonaPluginName
	}
	if err := overLimit(ctx, tx, MaxPersonaPlugins, "plugins per persona", `SELECT count(*) FROM room_persona_plugins WHERE persona_id = ?`, personaID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO room_persona_plugins(persona_id, plugin_id) VALUES(?,?)`, personaID, pluginID); err != nil {
		return fmt.Errorf("activate plugin: %w", err)
	}
	return nil
}

// AddPluginVersion stores a new version of plugin id from its re-resolved ref, waiting for
// inspection; the approved version keeps running meanwhile (009-AC11). ErrUpToDate when the
// latest version has that commit, unless it came with an import: then the same commit fetched
// from the repo replaces the archive's files (009-AC27). ErrNotFound, ErrRoomFull.
func (s *Store) AddPluginVersion(ctx context.Context, roomID string, id int64, refName string, v PluginSnapshot, by *Participant, defaultQuota int64) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "add plugin version", func(tx *txn) (err error) {
		m, err = addPluginVersion(ctx, tx, roomID, id, refName, v, by, defaultQuota)
		return err
	})
	return m, err
}

func addPluginVersion(ctx context.Context, tx *txn, roomID string, id int64, refName string, v PluginSnapshot, by *Participant, defaultQuota int64) (*Message, error) {
	var m *Message
	err := func() error {
		var name, commit string
		var n int
		var imported bool
		err := tx.QueryRowContext(ctx, `SELECT p.name, v.n, v.commit_sha, v.hold FROM room_plugins p JOIN plugin_versions v ON v.plugin_id = p.id
			WHERE p.id = ? AND p.room_id = ? AND p.removed_at IS NULL ORDER BY v.n DESC LIMIT 1`, id, roomID).Scan(&name, &n, &commit, &imported)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("add plugin version: %w", err)
		}
		if commit == v.Commit && !imported {
			return ErrUpToDate
		}
		if err := pluginsFit(ctx, tx, roomID, int64(len(v.Data)), defaultQuota); err != nil {
			return err
		}
		if err := insertPluginVersion(ctx, tx, id, n+1, v, by); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE room_plugins SET ref_name = ? WHERE id = ?`, refName, id); err != nil {
			return fmt.Errorf("add plugin version: %w", err)
		}
		m, err = memberLine(ctx, tx, roomID, by, fmt.Sprintf("updated the plugin %s to v%d, %s @ %s.", name, n+1, refName, shortCommit(v.Commit)))
		return err
	}()
	return m, err
}

// RemovePlugin takes plugin id off every persona of room from their next turn; its versions go
// once no running turn loads one (009-AC12). ErrNotFound.
func (s *Store) RemovePlugin(ctx context.Context, roomID string, id int64, by *Participant) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "remove plugin", func(tx *txn) error {
		var name string
		err := tx.QueryRowContext(ctx, `UPDATE room_plugins SET removed_at = ? WHERE id = ? AND room_id = ? AND removed_at IS NULL RETURNING name`,
			now(), id, roomID).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("remove plugin: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM room_persona_plugins WHERE plugin_id = ?`, id); err != nil {
			return fmt.Errorf("remove plugin: %w", err)
		}
		// An update waiting for the AI host would find no plugin to add to.
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_fetches WHERE plugin_id = ?`, id); err != nil {
			return fmt.Errorf("remove plugin: %w", err)
		}
		if _, err := tx.ExecContext(ctx, purgePlugins); err != nil {
			return fmt.Errorf("remove plugin: %w", err)
		}
		m, err = memberLine(ctx, tx, roomID, by, "removed the plugin "+name+".")
		return err
	})
	return m, err
}

// SetPersonaPlugin turns plugin pluginID on or off for persona personaID of room (009-AC10).
// ErrNotFound, ErrLimit, ErrPersonaPluginName.
func (s *Store) SetPersonaPlugin(ctx context.Context, roomID string, personaID, pluginID int64, on bool) error {
	return s.withTx(ctx, "set persona plugin", func(tx *txn) error {
		if err := personaInRoom(ctx, tx, roomID, personaID); err != nil {
			return err
		}
		var name string
		err := tx.QueryRowContext(ctx, `SELECT name FROM room_plugins WHERE id = ? AND room_id = ? AND removed_at IS NULL`, pluginID, roomID).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("set persona plugin: %w", err)
		}
		if on {
			return activatePlugin(ctx, tx, personaID, pluginID, name)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM room_persona_plugins WHERE persona_id = ? AND plugin_id = ?`, personaID, pluginID); err != nil {
			return fmt.Errorf("set persona plugin: %w", err)
		}
		return nil
	})
}

// DecidePluginVersion is the host's answer to version n of plugin id, pending under "approve
// everything" (009-AC14); the chat says so. ErrForbidden unless by hosts the room, ErrNotFound,
// ErrPluginState unless the version is pending and newer than the running one.
func (s *Store) DecidePluginVersion(ctx context.Context, roomID string, id int64, n int, approve bool, by *Participant) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "decide plugin version", func(tx *txn) error {
		if err := asHost(ctx, tx, roomID, by); err != nil {
			return err
		}
		var name, state string
		var superseded bool
		err := tx.QueryRowContext(ctx, `SELECT p.name, v.state, EXISTS (SELECT 1 FROM plugin_versions w WHERE w.plugin_id = p.id AND w.n > v.n AND w.state = 'approved')
			FROM plugin_versions v JOIN room_plugins p ON p.id = v.plugin_id
			WHERE p.id = ? AND p.room_id = ? AND p.removed_at IS NULL AND v.n = ?`, id, roomID, n).Scan(&name, &state, &superseded)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("decide plugin version: %w", err)
		}
		// An older version than the running one never loads again, and its snapshot is gone.
		if state != PluginPending || superseded {
			return ErrPluginState
		}
		state, body := PluginApproved, fmt.Sprintf("approved the plugin %s v%d.", name, n)
		if !approve {
			state, body = PluginRefused, fmt.Sprintf("refused the plugin %s v%d: it does not load.", name, n)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE plugin_versions SET state = ?, decided_by_name = ?, decided_at = ? WHERE plugin_id = ? AND n = ?`,
			state, by.Name, now(), id, n); err != nil {
			return fmt.Errorf("decide plugin version: %w", err)
		}
		if _, err := tx.ExecContext(ctx, dropSuperseded); err != nil {
			return fmt.Errorf("decide plugin version: %w", err)
		}
		m, err = memberLine(ctx, tx, roomID, by, body)
		return err
	})
	return m, err
}

// InspectingVersions lists the ids of room's versions waiting for the host's daemon (009-AC13).
func (s *Store) InspectingVersions(ctx context.Context, roomID string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT v.id FROM plugin_versions v JOIN room_plugins p ON p.id = v.plugin_id
		WHERE p.room_id = ? AND p.removed_at IS NULL AND v.state = 'inspecting' ORDER BY v.id`, roomID)
	if err != nil {
		return nil, fmt.Errorf("inspecting versions: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("inspecting versions: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PluginVersionData returns the snapshot tar of version id, a version of a plugin of room, empty
// once dropSuperseded deleted it. ErrNotFound.
func (s *Store) PluginVersionData(ctx context.Context, roomID string, id int64) ([]byte, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(sn.data, ''::bytea) FROM plugin_versions v JOIN room_plugins p ON p.id = v.plugin_id
		LEFT JOIN plugin_snapshots sn ON sn.version_id = v.id
		WHERE v.id = ? AND p.room_id = ? AND p.removed_at IS NULL`, id, roomID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("plugin version data: %w", err)
	}
	return data, nil
}

// pluginHostSender signs the lines about the host daemon's inspection.
const pluginHostSender = "AI host"

// ReportPluginVersion records the host daemon's inspection of version id (009-AC13): a failed
// validation makes it invalid, with reason; a passed one waits for the host while their daemon
// approves everything or the version came with an import (009-AC27), else is approved at once. The chat says which. It returns the new state.
// ErrForbidden unless daemonID is the room's host daemon, checked in the transaction: a hand-over
// meanwhile must not let the former host's daemon decide. ErrNotFound, ErrPluginState unless the
// version waits for inspection.
func (s *Store) ReportPluginVersion(ctx context.Context, roomID string, id, daemonID int64, ok bool, report json.RawMessage, reason string) (*Message, string, error) {
	var m *Message
	var state string
	err := s.withTx(ctx, "report plugin version", func(tx *txn) error {
		var name string
		var n int
		var hold bool
		err := tx.QueryRowContext(ctx, `SELECT p.name, v.n, v.state, v.hold FROM plugin_versions v JOIN room_plugins p ON p.id = v.plugin_id
			WHERE v.id = ? AND p.room_id = ? AND p.removed_at IS NULL`, id, roomID).Scan(&name, &n, &state, &hold)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("report plugin version: %w", err)
		}
		if state != PluginInspecting {
			return ErrPluginState
		}
		var approveAll bool
		err = tx.QueryRowContext(ctx, `SELECT d.approve_all FROM rooms r JOIN daemons d ON d.participant_id = r.host_participant_id
			WHERE r.id = ? AND d.id = ?`, roomID, daemonID).Scan(&approveAll)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrForbidden
		}
		if err != nil {
			return fmt.Errorf("approval mode: %w", err)
		}
		var body string
		var decided any
		switch {
		case !ok:
			state, reason = PluginInvalid, "refused by Claude Code: "+reason
			body = fmt.Sprintf("The plugin %s v%d was %s.", name, n, reason)
		case approveAll || hold:
			state, reason = PluginPending, ""
			body = fmt.Sprintf("The AI host checked the plugin %s v%d.%s", name, n, waitsNote)
		default:
			state, reason, decided = PluginApproved, "", now()
			body = fmt.Sprintf("The AI host checked the plugin %s v%d: approved automatically.", name, n)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE plugin_versions SET state = ?, reason = ?, report = ?, decided_at = ? WHERE id = ?`,
			state, reason, string(report), decided, id); err != nil {
			return fmt.Errorf("report plugin version: %w", err)
		}
		if _, err := tx.ExecContext(ctx, dropSuperseded); err != nil {
			return fmt.Errorf("report plugin version: %w", err)
		}
		m = &Message{RoomID: roomID, Kind: KindSystem, Sender: pluginHostSender, Body: body}
		return insertMessage(ctx, tx, m)
	})
	return m, state, err
}

// pinTurnPlugins copies onto turn id, as it starts, the running version of each plugin active for
// its persona: a version approved later loads from the next turn (009-AC17, 009-AC18).
func pinTurnPlugins(ctx context.Context, tx *txn, id, personaID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE turns SET plugins = (
			SELECT COALESCE(jsonb_agg(jsonb_build_object('name', p.name, 'version_id', v.id, 'n', v.n, 'sha256', v.sha256) ORDER BY p.name), '[]')::text
				FROM room_persona_plugins k
				JOIN room_plugins p ON p.id = k.plugin_id AND p.removed_at IS NULL
				JOIN plugin_versions v ON v.id = (SELECT id FROM plugin_versions WHERE plugin_id = p.id AND state = 'approved' ORDER BY n DESC LIMIT 1)
				WHERE k.persona_id = ?)
		WHERE id = ?`, personaID, id)
	if err != nil {
		return fmt.Errorf("pin turn plugins: %w", err)
	}
	return nil
}

// TurnPlugin is a plugin version a turn loads. Manifest and PluginID are read with it: the job
// names the options the daemon needs, under the plugin's id (009-AC22, 009-AC24).
type TurnPlugin struct {
	Name      string          `json:"name"`
	VersionID int64           `json:"version_id"`
	N         int             `json:"n"`
	SHA256    string          `json:"sha256"`
	Manifest  json.RawMessage `json:"-"`
	PluginID  int64           `json:"-"`
}

// TurnPlugins returns the plugin versions turn id of room loads, by name.
func (s *Store) TurnPlugins(ctx context.Context, roomID string, id int64) ([]TurnPlugin, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT plugins FROM turns WHERE id = ? AND room_id = ?`, id, roomID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("turn plugins: %w", err)
	}
	var out []TurnPlugin
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("turn plugins: %w", err)
	}
	for i, p := range out {
		var m string
		err := s.db.QueryRowContext(ctx, `SELECT manifest, plugin_id FROM plugin_versions WHERE id = ?`, p.VersionID).Scan(&m, &out[i].PluginID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("turn plugins: %w", err)
		}
		out[i].Manifest = json.RawMessage(m)
	}
	return out, nil
}

// SetPluginsSetup records, from a turn's result, which of the plugins it pinned the daemon left
// out for an option the host has not set, and which loaded (009-AC24). Names the turn did not pin
// keep their state.
func (s *Store) SetPluginsSetup(ctx context.Context, roomID string, pinned, skipped []string) (bool, error) {
	changed := false
	err := s.withTx(ctx, "set plugins setup", func(tx *txn) error {
		for _, name := range pinned {
			need := slices.Contains(skipped, name)
			res, err := tx.ExecContext(ctx, `UPDATE room_plugins SET needs_setup = ? WHERE room_id = ? AND name = ? AND removed_at IS NULL AND needs_setup != ?`,
				need, roomID, name, need)
			if err != nil {
				return fmt.Errorf("set plugins setup: %w", err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				changed = true
			}
		}
		return nil
	})
	return changed, err
}

// TurnPluginData returns the snapshot tar with that sha256 among those turn id of room loads.
// ErrNotFound.
func (s *Store) TurnPluginData(ctx context.Context, roomID string, id int64, sha256 string) ([]byte, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT sn.data FROM turns t CROSS JOIN jsonb_array_elements(t.plugins::jsonb) j(value)
		JOIN plugin_versions v ON v.id = (j.value->>'version_id')::bigint
		JOIN plugin_snapshots sn ON sn.version_id = v.id
		WHERE t.id = ? AND t.room_id = ? AND v.sha256 = ? AND j.value->>'sha256' = ? LIMIT 1`, id, roomID, sha256, sha256).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("turn plugin data: %w", err)
	}
	return data, nil
}

// pluginsVersionSQL reads only indexes: plugin_versions_state covers the version ids and states.
const pluginsVersionSQL = `SELECT COALESCE(string_agg(k, ',' ORDER BY k), '') FROM (
	SELECT v.id || v.state AS k FROM plugin_versions v JOIN room_plugins p ON p.id = v.plugin_id
		WHERE p.room_id = ? AND p.removed_at IS NULL
	UNION ALL SELECT 'a' || pp.persona_id || '.' || pp.plugin_id FROM room_persona_plugins pp JOIN room_plugins p ON p.id = pp.plugin_id
		WHERE p.room_id = ? AND p.removed_at IS NULL
	UNION ALL SELECT 's' || id FROM room_plugins WHERE room_id = ? AND removed_at IS NULL AND needs_setup = 1
	UNION ALL SELECT 'f' || id || state FROM plugin_fetches WHERE room_id = ?) ks`

// PluginsVersion changes whenever the room's plugin list does: a plugin added or removed, a
// version added or moved to another state, a persona's activation, a plugin needing host setup or
// no more, a fetch through the AI host asked, failed or done. The room stream sends a
// "plugins" event on a change rather than the list itself, which holds manifests, reports and change summaries.
func (s *Store) PluginsVersion(ctx context.Context, roomID string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, pluginsVersionSQL, roomID, roomID, roomID, roomID).Scan(&v)
	if err != nil {
		return "", fmt.Errorf("plugins version: %w", err)
	}
	return v, nil
}

// RoomPlugins lists the room's plugins by name, removed ones aside.
func (s *Store) RoomPlugins(ctx context.Context, roomID string) ([]RoomPlugin, error) {
	return s.roomPlugins(ctx, roomID, 0)
}

// RoomPlugin returns plugin id of room. ErrNotFound, also once removed.
func (s *Store) RoomPlugin(ctx context.Context, roomID string, id int64) (*RoomPlugin, error) {
	ps, err := s.roomPlugins(ctx, roomID, id)
	if err != nil {
		return nil, err
	}
	if len(ps) == 0 {
		return nil, ErrNotFound
	}
	return &ps[0], nil
}

func (s *Store) roomPlugins(ctx context.Context, roomID string, id int64) ([]RoomPlugin, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, source_url, ref, ref_name, entry, private, needs_setup, created_by_name, created_at
		FROM room_plugins WHERE room_id = ? AND removed_at IS NULL AND (? = 0 OR id = ?) ORDER BY name`, roomID, id, id)
	if err != nil {
		return nil, fmt.Errorf("room plugins: %w", err)
	}
	out := []RoomPlugin{}
	byID := map[int64]int{}
	for rows.Next() {
		var p RoomPlugin
		var created string
		if err := rows.Scan(&p.ID, &p.Name, &p.URL, &p.Ref, &p.RefName, &p.Entry, &p.Private, &p.NeedsSetup, &p.CreatedBy, &created); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("room plugins: %w", err)
		}
		p.CreatedAt, p.Versions, p.Personas = parseTS(created), []PluginVersion{}, []int64{}
		byID[p.ID] = len(out)
		out = append(out, p)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("room plugins: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}
	rows, err = s.db.QueryContext(ctx, `SELECT v.plugin_id, v.id, v.n, v.commit_sha, v.sha256, v.size, v.files, v.file_list, v.manifest,
		COALESCE(v.report, ''), v.state, v.reason, v.added_by_name, v.added_at, COALESCE(v.decided_by_name, ''), v.decided_at, v.hold
		FROM plugin_versions v JOIN room_plugins p ON p.id = v.plugin_id WHERE p.room_id = ? AND p.removed_at IS NULL ORDER BY v.plugin_id, v.n`, roomID)
	if err != nil {
		return nil, fmt.Errorf("plugin versions: %w", err)
	}
	for rows.Next() {
		var pid int64
		var v PluginVersion
		var files, manifest, report, added string
		var decided sql.NullString
		if err := rows.Scan(&pid, &v.ID, &v.N, &v.Commit, &v.SHA256, &v.Size, &v.Files, &files, &manifest, &report, &v.State, &v.Reason,
			&v.AddedBy, &added, &v.DecidedBy, &decided, &v.Imported); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("plugin versions: %w", err)
		}
		i, ok := byID[pid]
		if !ok {
			continue
		}
		v.FileList, v.Manifest, v.AddedAt, v.DecidedAt = json.RawMessage(files), json.RawMessage(manifest), parseTS(added), parseNullTS(decided)
		if report != "" {
			v.Report = json.RawMessage(report)
		}
		out[i].Versions = append(out[i].Versions, v)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("plugin versions: %w", err)
	}
	rows, err = s.db.QueryContext(ctx, `SELECT k.plugin_id, k.persona_id FROM room_persona_plugins k JOIN room_plugins p ON p.id = k.plugin_id
		WHERE p.room_id = ? ORDER BY k.persona_id`, roomID)
	if err != nil {
		return nil, fmt.Errorf("plugin personas: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pid, persona int64
		if err := rows.Scan(&pid, &persona); err != nil {
			return nil, fmt.Errorf("plugin personas: %w", err)
		}
		if i, ok := byID[pid]; ok {
			out[i].Personas = append(out[i].Personas, persona)
		}
	}
	return out, rows.Err()
}
