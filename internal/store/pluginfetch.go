package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Fetch request states (009-AC19): waiting for the AI host's Fetch, or failed with a reason the
// room shows until a room admin dismisses it. A fetch that worked is deleted.
const (
	FetchWaiting = "waiting"
	FetchFailed  = "failed"
)

// MaxRoomFetches bounds the fetch requests a room keeps, waiting or failed.
const MaxRoomFetches = 10

// ErrFetchWaiting is returned when the plugin has an update waiting for the AI host already.
var ErrFetchWaiting = errors.New("an update of this plugin already waits for the AI host")

// PluginFetch is a request that the AI host fetch a repo the server cannot read (009-AC19). Plugin
// names the private plugin it updates, 0 for an add.
type PluginFetch struct {
	ID          int64     `json:"id"`
	PluginID    int64     `json:"plugin_id,omitempty"`
	PluginName  string    `json:"plugin_name,omitempty"`
	URL         string    `json:"url"`
	Ref         string    `json:"ref"`
	Entries     []string  `json:"entries"`
	Personas    []int64   `json:"personas"`
	State       string    `json:"state"`
	Reason      string    `json:"reason,omitempty"`
	RequestedBy string    `json:"requested_by"`
	CreatedAt   time.Time `json:"created_at"`
	// requestedByID is the participant the plugin is added as, 0 once gone.
	requestedByID int64
}

// NewPluginFetch is what a room admin asks the AI host to fetch.
type NewPluginFetch struct {
	PluginID int64
	URL, Ref string
	Entries  []string
	Personas []int64
}

// AddPluginFetch records a request that the AI host fetch a repo, and says so in the chat
// (009-AC19, 009-AC21). ErrNotFound for a persona not in the room or a plugin not in it, ErrLimit,
// ErrFetchWaiting.
func (s *Store) AddPluginFetch(ctx context.Context, roomID string, f NewPluginFetch, by *Participant) (*PluginFetch, *Message, error) {
	var out *PluginFetch
	var m *Message
	err := s.withTx(ctx, "add plugin fetch", func(tx *txn) error {
		if err := overLimit(ctx, tx, MaxRoomFetches, "requests to the AI host per room, waiting or failed",
			`SELECT count(*) FROM plugin_fetches WHERE room_id = ?`, roomID); err != nil {
			return err
		}
		for _, pid := range f.Personas {
			if err := personaInRoom(ctx, tx, roomID, pid); err != nil {
				return err
			}
		}
		body := "asked the AI host to fetch a plugin from " + f.URL + "."
		if f.PluginID != 0 {
			var name string
			err := tx.QueryRowContext(ctx, `SELECT name FROM room_plugins WHERE id = ? AND room_id = ? AND removed_at IS NULL`, f.PluginID, roomID).Scan(&name)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return fmt.Errorf("add plugin fetch: %w", err)
			}
			body = "asked the AI host to fetch an update of the plugin " + name + "."
		}
		entries, _ := json.Marshal(nonNil(f.Entries)) // strings: cannot fail
		personas, _ := json.Marshal(nonNil(f.Personas))
		var id int64
		err := tx.QueryRowContext(ctx, `INSERT INTO plugin_fetches(room_id, plugin_id, source_url, ref, entries, personas, state,
			requested_by_name, requested_by_id, created_at) VALUES(?,?,?,?,?,?,?,?,?,?) RETURNING id`,
			roomID, nullID(f.PluginID), f.URL, f.Ref, string(entries), string(personas), FetchWaiting, by.Name, nullID(by.ID), now()).Scan(&id)
		if isUnique(err) {
			return ErrFetchWaiting
		}
		if err != nil {
			return fmt.Errorf("add plugin fetch: %w", err)
		}
		if m, err = memberLine(ctx, tx, roomID, by, body); err != nil {
			return err
		}
		out, err = pluginFetch(ctx, tx, roomID, id)
		return err
	})
	return out, m, err
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

const fetchColumns = `SELECT f.id, COALESCE(f.plugin_id, 0), COALESCE(p.name, ''), f.source_url, f.ref, f.entries, f.personas, f.state,
	f.reason, f.requested_by_name, COALESCE(f.requested_by_id, 0), f.created_at FROM plugin_fetches f LEFT JOIN room_plugins p ON p.id = f.plugin_id`

type rowScanner interface{ Scan(dest ...any) error }

func scanFetch(r rowScanner) (*PluginFetch, error) {
	var f PluginFetch
	var entries, personas, created string
	if err := r.Scan(&f.ID, &f.PluginID, &f.PluginName, &f.URL, &f.Ref, &entries, &personas, &f.State, &f.Reason, &f.RequestedBy, &f.requestedByID, &created); err != nil {
		return nil, err
	}
	// Written by AddPluginFetch from JSON arrays.
	_ = json.Unmarshal([]byte(entries), &f.Entries)
	_ = json.Unmarshal([]byte(personas), &f.Personas)
	f.Entries, f.Personas, f.CreatedAt = nonNil(f.Entries), nonNil(f.Personas), parseTS(created)
	return &f, nil
}

func pluginFetch(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *row
}, roomID string, id int64) (*PluginFetch, error) {
	f, err := scanFetch(q.QueryRowContext(ctx, fetchColumns+` WHERE f.id = ? AND f.room_id = ?`, id, roomID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("plugin fetch: %w", err)
	}
	return f, nil
}

// PluginFetch returns fetch request id of room. ErrNotFound.
func (s *Store) PluginFetch(ctx context.Context, roomID string, id int64) (*PluginFetch, error) {
	return pluginFetch(ctx, s.db, roomID, id)
}

// PluginFetches lists room's fetch requests, oldest first; waitingOnly leaves the failed ones out.
func (s *Store) PluginFetches(ctx context.Context, roomID string, waitingOnly bool) ([]PluginFetch, error) {
	rows, err := s.db.QueryContext(ctx, fetchColumns+` WHERE f.room_id = ? AND (? = 0 OR f.state = 'waiting') ORDER BY f.id`, roomID, waitingOnly)
	if err != nil {
		return nil, fmt.Errorf("plugin fetches: %w", err)
	}
	defer rows.Close()
	out := []PluginFetch{}
	for rows.Next() {
		f, err := scanFetch(rows)
		if err != nil {
			return nil, fmt.Errorf("plugin fetches: %w", err)
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// DeletePluginFetch cancels a waiting request or dismisses a failed one. ErrNotFound.
func (s *Store) DeletePluginFetch(ctx context.Context, roomID string, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM plugin_fetches WHERE id = ? AND room_id = ?`, id, roomID)
	if err != nil {
		return fmt.Errorf("delete plugin fetch: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// FailPluginFetch marks waiting request id failed with reason, and the chat says so as the AI
// host (009-AC21). ErrNotFound, ErrPluginState unless it waits.
func (s *Store) FailPluginFetch(ctx context.Context, roomID string, id int64, reason string) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "fail plugin fetch", func(tx *txn) error {
		f, err := waitingFetch(ctx, tx, roomID, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE plugin_fetches SET state = ?, reason = ? WHERE id = ?`, FetchFailed, reason, id); err != nil {
			return fmt.Errorf("fail plugin fetch: %w", err)
		}
		what := "the plugin from " + f.URL
		if f.PluginName != "" {
			what = "the update of " + f.PluginName
		}
		m = &Message{RoomID: roomID, Kind: KindSystem, Sender: pluginHostSender, Body: fmt.Sprintf("The AI host could not fetch %s: %s.", what, strings.TrimRight(reason, "."))}
		return insertMessage(ctx, tx, m)
	})
	return m, err
}

func waitingFetch(ctx context.Context, tx *txn, roomID string, id int64) (*PluginFetch, error) {
	f, err := pluginFetch(ctx, tx, roomID, id)
	if err != nil {
		return nil, err
	}
	if f.State != FetchWaiting {
		return nil, ErrPluginState
	}
	return f, nil
}

// CompletePluginFetch stores what the AI host fetched for waiting request id, marked private, and
// deletes the request, in one transaction (009-AC20, 009-AC21). The plugins are added as the
// participant who asked. An add adds ps on the request's personas still in the room; an update
// adds ps[0] as the plugin's new version, or with the latest version's commit, only says it is up
// to date. The errors of AddPlugins and AddPluginVersion.
func (s *Store) CompletePluginFetch(ctx context.Context, roomID string, id int64, ps []NewPlugin, defaultQuota int64) (msgs []*Message, upToDate bool, err error) {
	err = s.withTx(ctx, "complete plugin fetch", func(tx *txn) error {
		f, err := waitingFetch(ctx, tx, roomID, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_fetches WHERE id = ?`, id); err != nil {
			return fmt.Errorf("complete plugin fetch: %w", err)
		}
		by := &Participant{ID: f.requestedByID, Name: f.RequestedBy}
		if f.PluginID != 0 {
			if len(ps) != 1 {
				return fmt.Errorf("complete plugin fetch: an update takes one plugin, not %d", len(ps))
			}
			m, err := addPluginVersion(ctx, tx, roomID, f.PluginID, ps[0].RefName, ps[0].PluginSnapshot, by, defaultQuota)
			if errors.Is(err, ErrUpToDate) {
				upToDate = true
				m = &Message{RoomID: roomID, Kind: KindSystem, Sender: pluginHostSender, Body: "The AI host fetched the plugin " + f.PluginName + ": up to date."}
				err = insertMessage(ctx, tx, m)
			}
			if err != nil {
				return err
			}
			msgs = append(msgs, m)
			return nil
		}
		var personas []int64
		for _, pid := range f.Personas {
			if err := personaInRoom(ctx, tx, roomID, pid); err == nil {
				personas = append(personas, pid)
			} else if !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		for i := range ps {
			ps[i].Private = true
		}
		msgs, err = addPlugins(ctx, tx, roomID, ps, personas, by, defaultQuota)
		return err
	})
	return msgs, upToDate, err
}
