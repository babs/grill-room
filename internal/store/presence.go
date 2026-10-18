package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// PresenceRow is a row of the presence pods share: a participant's person row (Tab "": since when
// they are here, Beat their last sign of life) or one of their open tabs.
type PresenceRow struct {
	Room                             string
	ParticipantID                    int64
	Tab                              string
	SinceMS, BeatMS, DeviceMS        int64
	Audio, Mic, Muted, Deaf, Focused bool
	ExpiresMS                        int64
}

// PresenceTimes are the bounds a presence write applies, in ms: Grace keeps a quiet participant
// here, Forget keeps a gone one's last-seen time, Lapse holds a device flag.
type PresenceTimes struct{ Now, Grace, Forget, Lapse int64 }

// dropExpiredPerson drops pid's rows expired at nowMS, so a revived tab starts clean.
func dropExpiredPerson(ctx context.Context, t *txn, room string, pid, nowMS int64) error {
	_, err := t.ExecContext(ctx, `DELETE FROM presence WHERE room_id = ? AND participant_id = ? AND expires_at_ms <= ?`, room, pid, nowMS)
	return err
}

// upsertPerson upserts pid's person row: their last sign of life at Now and, with arrive, the start
// of a stay unless they are here already. It first drops pid's expired rows.
func upsertPerson(ctx context.Context, t *txn, room string, pid int64, at PresenceTimes, arrive bool) error {
	if err := dropExpiredPerson(ctx, t, room, pid, at.Now); err != nil {
		return err
	}
	since := int64(0)
	if arrive {
		since = at.Now
	}
	_, err := t.ExecContext(ctx, `INSERT INTO presence (room_id, participant_id, tab, since_at_ms, beat_at_ms, expires_at_ms)
		VALUES (?1, ?2, '', ?3, ?4, ?5)
		ON CONFLICT (room_id, participant_id, tab) DO UPDATE SET
			since_at_ms = CASE WHEN CAST(?3 AS BIGINT) > 0 AND presence.beat_at_ms < ?6 AND NOT EXISTS (SELECT 1 FROM presence t
				WHERE t.room_id = ?1 AND t.participant_id = ?2 AND t.tab <> '' AND t.beat_at_ms >= ?6) THEN ?3 ELSE presence.since_at_ms END,
			beat_at_ms = ?4, expires_at_ms = GREATEST(presence.expires_at_ms, ?5)`,
		room, pid, since, at.Now, at.Now+at.Forget, at.Now-at.Grace)
	return err
}

// PresenceOpen records a stream of tab open at Now: pid arrives and the tab lives for the grace.
// A stream renews it with the same call.
func (s *Store) PresenceOpen(ctx context.Context, room string, pid int64, tab string, at PresenceTimes) error {
	err := s.db.unlocked(ctx, func(t *txn) error {
		if err := lockPresence(ctx, t, room, pid); err != nil {
			return err
		}
		if err := upsertPerson(ctx, t, room, pid, at, true); err != nil {
			return err
		}
		_, err := t.ExecContext(ctx, `INSERT INTO presence (room_id, participant_id, tab, beat_at_ms, expires_at_ms) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (room_id, participant_id, tab) DO UPDATE SET beat_at_ms = excluded.beat_at_ms,
				expires_at_ms = GREATEST(presence.expires_at_ms, excluded.expires_at_ms)`,
			room, pid, tab, at.Now, at.Now+at.Grace)
		return err
	})
	if err != nil {
		return fmt.Errorf("presence open: %w", err)
	}
	return nil
}

// PresenceRelease records tab's stream closed at Now: pid and the tab stay for the grace.
func (s *Store) PresenceRelease(ctx context.Context, room string, pid int64, tab string, at PresenceTimes) error {
	err := s.db.unlocked(ctx, func(t *txn) error {
		if err := lockPresence(ctx, t, room, pid); err != nil {
			return err
		}
		if err := upsertPerson(ctx, t, room, pid, at, false); err != nil {
			return err
		}
		_, err := t.ExecContext(ctx, `UPDATE presence SET beat_at_ms = ?, expires_at_ms = GREATEST(expires_at_ms, ?)
			WHERE room_id = ? AND participant_id = ? AND tab = ?`, at.Now, at.Now+at.Grace, room, pid, tab)
		return err
	})
	if err != nil {
		return fmt.Errorf("presence release: %w", err)
	}
	return nil
}

// TabState is the audio state a tab's heartbeat reports.
type TabState struct{ Audio, Mic, Muted, Deaf, Focused bool }

// tabRoom is the clause admitting a tab row: the tab is known, or pid holds fewer than ?4 tabs.
// Params: ?1 room, ?2 pid, ?3 tab, ?4 maxTabs.
const tabRoom = `WHERE EXISTS (SELECT 1 FROM presence WHERE room_id = ?1 AND participant_id = ?2 AND tab = ?3)
	OR (SELECT count(*) FROM presence WHERE room_id = ?1 AND participant_id = ?2 AND tab <> '') < ?4`

// lockPresence serialises the presence writes of pid in room: the maxTabs check is exact, and
// their row locks never interleave into a deadlock.
func lockPresence(ctx context.Context, t *txn, room string, pid int64) error {
	return xactLock(ctx, t, LockPresence, room+" "+strconv.FormatInt(pid, 10))
}

// PresenceBeat records tab's heartbeat at Now with its audio state; false when the tab is new and
// pid holds maxTabs tabs already. pid arrives either way. With gone the tab closes instead: its row
// goes, while pid stays for the grace.
func (s *Store) PresenceBeat(ctx context.Context, room string, pid int64, tab string, st TabState, gone bool, maxTabs int, at PresenceTimes) (bool, error) {
	ok := true
	err := s.db.unlocked(ctx, func(t *txn) error {
		if err := lockPresence(ctx, t, room, pid); err != nil {
			return err
		}
		if err := upsertPerson(ctx, t, room, pid, at, !gone); err != nil {
			return err
		}
		if gone {
			_, err := t.ExecContext(ctx, `DELETE FROM presence WHERE room_id = ? AND participant_id = ? AND tab = ?`, room, pid, tab)
			return err
		}
		res, err := t.ExecContext(ctx, `INSERT INTO presence (room_id, participant_id, tab, beat_at_ms, audio, mic, muted, deaf, focused, expires_at_ms)
			SELECT CAST(?1 AS TEXT), CAST(?2 AS BIGINT), CAST(?3 AS TEXT), CAST(?5 AS BIGINT), CAST(?6 AS SMALLINT), CAST(?7 AS SMALLINT),
				CAST(?8 AS SMALLINT), CAST(?9 AS SMALLINT), CAST(?10 AS SMALLINT), CAST(?11 AS BIGINT) `+tabRoom+`
			ON CONFLICT (room_id, participant_id, tab) DO UPDATE SET beat_at_ms = excluded.beat_at_ms, audio = excluded.audio,
				mic = excluded.mic, muted = excluded.muted, deaf = excluded.deaf, focused = excluded.focused,
				expires_at_ms = GREATEST(presence.expires_at_ms, excluded.expires_at_ms)`,
			room, pid, tab, maxTabs, at.Now, st.Audio, st.Mic, st.Muted, st.Deaf, st.Focused, at.Now+at.Grace)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		ok = n > 0
		return err
	})
	if err != nil {
		return false, fmt.Errorf("presence beat: %w", err)
	}
	return ok, nil
}

// PresenceDevice records tab's device transcription beat at Now: on holds the flag for the lapse,
// off drops it at once. False when the tab is new and pid holds maxTabs tabs already.
func (s *Store) PresenceDevice(ctx context.Context, room string, pid int64, tab string, on bool, maxTabs int, at PresenceTimes) (bool, error) {
	ok := true
	err := s.db.unlocked(ctx, func(t *txn) error {
		if err := lockPresence(ctx, t, room, pid); err != nil {
			return err
		}
		if err := dropExpiredPerson(ctx, t, room, pid, at.Now); err != nil {
			return err
		}
		if !on {
			_, err := t.ExecContext(ctx, `UPDATE presence SET device_at_ms = 0 WHERE room_id = ? AND participant_id = ? AND tab = ?`, room, pid, tab)
			return err
		}
		res, err := t.ExecContext(ctx, `INSERT INTO presence (room_id, participant_id, tab, device_at_ms, expires_at_ms)
			SELECT CAST(?1 AS TEXT), CAST(?2 AS BIGINT), CAST(?3 AS TEXT), CAST(?5 AS BIGINT), CAST(?6 AS BIGINT) `+tabRoom+`
			ON CONFLICT (room_id, participant_id, tab) DO UPDATE SET device_at_ms = excluded.device_at_ms,
				expires_at_ms = GREATEST(presence.expires_at_ms, excluded.expires_at_ms)`,
			room, pid, tab, maxTabs, at.Now, at.Now+at.Lapse)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		ok = n > 0
		return err
	})
	if err != nil {
		return false, fmt.Errorf("presence device: %w", err)
	}
	return ok, nil
}

// Presence returns the presence rows of room, of every room when room is "", live at nowMS.
func (s *Store) Presence(ctx context.Context, room string, nowMS int64) ([]PresenceRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT room_id, participant_id, tab, since_at_ms, beat_at_ms, device_at_ms,
		audio, mic, muted, deaf, focused, expires_at_ms FROM presence WHERE (room_id = ?1 OR ?1 = '') AND expires_at_ms > ?2`, room, nowMS)
	if err != nil {
		return nil, fmt.Errorf("presence: %w", err)
	}
	defer rows.Close()
	var out []PresenceRow
	for rows.Next() {
		var r PresenceRow
		if err := rows.Scan(&r.Room, &r.ParticipantID, &r.Tab, &r.SinceMS, &r.BeatMS, &r.DeviceMS,
			&r.Audio, &r.Mic, &r.Muted, &r.Deaf, &r.Focused, &r.ExpiresMS); err != nil {
			return nil, fmt.Errorf("presence: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("presence: %w", err)
	}
	return out, nil
}

// STTWorker is the last heartbeat of an STT worker: the rooms whose audio it is in, by room the
// participants whose track it transcribes, its engine and whether it reads room glossaries.
type STTWorker struct {
	Name      string
	Rooms     []string
	Voices    map[string][]int64
	Engine    string
	Glossary  bool
	ExpiresMS int64
}

// BeatSTTWorker records w's heartbeat, live until w.ExpiresMS; false when w is new and maxWorkers
// workers live at nowMS already.
func (s *Store) BeatSTTWorker(ctx context.Context, w STTWorker, maxWorkers int, nowMS int64) (bool, error) {
	rooms, _ := json.Marshal(w.Rooms)   // plain values cannot fail to marshal
	voices, _ := json.Marshal(w.Voices) // plain values cannot fail to marshal
	ok := false
	err := s.db.unlocked(ctx, func(t *txn) error {
		// Beats run one at a time, so the maxWorkers check is exact.
		if err := xactLock(ctx, t, LockSTTWorkers, ""); err != nil {
			return err
		}
		res, err := t.ExecContext(ctx, `INSERT INTO stt_workers (worker, rooms, voices, engine, glossary, expires_at_ms)
			SELECT CAST(?1 AS TEXT), CAST(?2 AS TEXT), CAST(?3 AS TEXT), CAST(?4 AS TEXT), CAST(?5 AS SMALLINT), CAST(?6 AS BIGINT)
			WHERE EXISTS (SELECT 1 FROM stt_workers WHERE worker = ?1 AND expires_at_ms > ?7)
				OR (SELECT count(*) FROM stt_workers WHERE expires_at_ms > ?7) < ?8
			ON CONFLICT (worker) DO UPDATE SET rooms = excluded.rooms, voices = excluded.voices, engine = excluded.engine,
				glossary = excluded.glossary, expires_at_ms = excluded.expires_at_ms`,
			w.Name, string(rooms), string(voices), w.Engine, w.Glossary, w.ExpiresMS, nowMS, maxWorkers)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		ok = n > 0
		return err
	})
	if err != nil {
		return false, fmt.Errorf("stt worker: %w", err)
	}
	return ok, nil
}

// STTWorkers returns the workers live at nowMS.
func (s *Store) STTWorkers(ctx context.Context, nowMS int64) ([]STTWorker, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT worker, rooms, voices, engine, glossary, expires_at_ms FROM stt_workers WHERE expires_at_ms > ?`, nowMS)
	if err != nil {
		return nil, fmt.Errorf("stt workers: %w", err)
	}
	defer rows.Close()
	var out []STTWorker
	for rows.Next() {
		var w STTWorker
		var rooms, voices string
		if err := rows.Scan(&w.Name, &rooms, &voices, &w.Engine, &w.Glossary, &w.ExpiresMS); err != nil {
			return nil, fmt.Errorf("stt workers: %w", err)
		}
		if err := json.Unmarshal([]byte(rooms), &w.Rooms); err != nil {
			return nil, fmt.Errorf("stt workers: %w", err)
		}
		if err := json.Unmarshal([]byte(voices), &w.Voices); err != nil {
			return nil, fmt.Errorf("stt workers: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stt workers: %w", err)
	}
	return out, nil
}
