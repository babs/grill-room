package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// TTSLine is one line a persona says in a room's call, waiting for the TTS service (013-AC14).
type TTSLine struct {
	Seq                      int64
	Room                     string
	PersonaID                int64
	Persona, Voice           string
	Rate                     float64
	Text                     string
	QuietMS, ResumeS, QueueS int
	QueuedAtMS               int64
}

const ttsLineCols = `seq, room_id, persona_id, persona, voice, rate, text, quiet_ms, resume_s, queue_s, queued_at_ms`

// ttsLive is the clause of the lines still within their queue budget at ?1.
const ttsLive = `queued_at_ms + queue_s * 1000 >= ?1`

// ttsExpired deletes the lines past their queue budget at ?1, returning whether each was sent.
const ttsExpired = `DELETE FROM tts_lines WHERE NOT (` + ttsLive + `) RETURNING sent`

// AddTTSLine queues l, numbered from the one sequence every pod shares, then keeps the newest
// keep lines. It returns l's seq and how many lines it dropped that the TTS service never took:
// expired at nowMS, or past keep.
func (s *Store) AddTTSLine(ctx context.Context, l TTSLine, keep int, nowMS int64) (seq, noService int64, err error) {
	err = s.withTx(ctx, "add tts line", func(tx *txn) error {
		n, err := dropTTSLines(ctx, tx, ttsExpired, nowMS)
		if err != nil {
			return fmt.Errorf("add tts line: %w", err)
		}
		noService += n
		if err := tx.QueryRowContext(ctx, `INSERT INTO tts_lines (room_id, persona_id, persona, voice, rate, text, quiet_ms, resume_s, queue_s, queued_at_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING seq`,
			l.Room, l.PersonaID, l.Persona, l.Voice, l.Rate, l.Text, l.QuietMS, l.ResumeS, l.QueueS, l.QueuedAtMS).Scan(&seq); err != nil {
			return fmt.Errorf("add tts line: %w", err)
		}
		n, err = dropTTSLines(ctx, tx, `DELETE FROM tts_lines WHERE seq <= (SELECT seq FROM tts_lines ORDER BY seq DESC OFFSET ? LIMIT 1) RETURNING sent`, keep)
		if err != nil {
			return fmt.Errorf("add tts line: %w", err)
		}
		noService += n
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return seq, noService, nil
}

// dropTTSLines runs a DELETE .. RETURNING sent and counts the lines it dropped unsent.
func dropTTSLines(ctx context.Context, tx *txn, query string, args ...any) (int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var n int64
	for rows.Next() {
		var sent bool
		if err := rows.Scan(&sent); err != nil {
			return 0, err
		}
		if !sent {
			n++
		}
	}
	return n, rows.Err()
}

// SweepTTSLines deletes the lines past their queue budget at nowMS and returns how many the TTS
// service never took. It takes the write lock only when one expired.
func (s *Store) SweepTTSLines(ctx context.Context, nowMS int64) (noService int64, err error) {
	var expired bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tts_lines WHERE NOT (`+ttsLive+`))`, nowMS).Scan(&expired); err != nil {
		return 0, fmt.Errorf("sweep tts lines: %w", err)
	}
	if !expired {
		return 0, nil
	}
	err = s.withTx(ctx, "sweep tts lines", func(tx *txn) error {
		if noService, err = dropTTSLines(ctx, tx, ttsExpired, nowMS); err != nil {
			return fmt.Errorf("sweep tts lines: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return noService, nil
}

// TTSLinesAfter returns the lines after seq still within their budget at nowMS, in order, and
// marks them sent.
func (s *Store) TTSLinesAfter(ctx context.Context, after, nowMS int64) ([]TTSLine, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+ttsLineCols+` FROM tts_lines WHERE seq > ?2 AND `+ttsLive+` ORDER BY seq`, nowMS, after)
	if err != nil {
		return nil, fmt.Errorf("tts lines: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TTSLine
	for rows.Next() {
		var l TTSLine
		if err := rows.Scan(&l.Seq, &l.Room, &l.PersonaID, &l.Persona, &l.Voice, &l.Rate, &l.Text, &l.QuietMS, &l.ResumeS, &l.QueueS, &l.QueuedAtMS); err != nil {
			return nil, fmt.Errorf("tts lines: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tts lines: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	// The same clause as the read: an expired line in the range stays unsent, so counts as no_service.
	_, err = s.db.ExecContext(ctx, `UPDATE tts_lines SET sent = 1 WHERE seq > ?2 AND seq <= ?3 AND sent = 0 AND `+ttsLive,
		nowMS, after, out[len(out)-1].Seq)
	if err != nil {
		return nil, fmt.Errorf("tts lines sent: %w", err)
	}
	return out, nil
}

// TTSCursor returns where a stream with no Last-Event-ID resumes, after the newest line the TTS
// service took (else before the oldest kept), and the sequence's last value, above every seq
// handed out.
func (s *Store) TTSCursor(ctx context.Context) (resume, last int64, err error) {
	err = s.db.QueryRowContext(ctx, `WITH sq AS (SELECT COALESCE(pg_sequence_last_value(CAST(pg_get_serial_sequence('tts_lines', 'seq') AS regclass)), 0) AS v)
		SELECT COALESCE(MAX(seq) FILTER (WHERE sent = 1), MIN(seq) - 1, (SELECT v FROM sq)), (SELECT v FROM sq) FROM tts_lines`).Scan(&resume, &last)
	if err != nil {
		return 0, 0, fmt.Errorf("tts cursor: %w", err)
	}
	return resume, last, nil
}

// TTSPod is the TTS service's last heartbeat and its stream's lease, from whichever pod got them.
type TTSPod struct {
	Pod         string
	Rooms, Bots []string
	// States is the TTS service's persona states, as it sent them.
	States                  json.RawMessage
	BeatAtMS, StreamUntilMS int64
}

// BeatTTSPod records a heartbeat of the TTS service at nowMS; the stream's lease stands.
func (s *Store) BeatTTSPod(ctx context.Context, p TTSPod, nowMS int64) error {
	rooms, _ := json.Marshal(p.Rooms) // plain values cannot fail to marshal
	bots, _ := json.Marshal(p.Bots)   // plain values cannot fail to marshal
	states := string(p.States)
	if states == "" {
		states = "[]"
	}
	err := s.db.unlocked(ctx, func(t *txn) error {
		_, err := t.ExecContext(ctx, `INSERT INTO tts_pod (id, pod, rooms, bots, states, beat_at_ms) VALUES (1, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET pod = excluded.pod, rooms = excluded.rooms, bots = excluded.bots, states = excluded.states, beat_at_ms = excluded.beat_at_ms`,
			p.Pod, string(rooms), string(bots), states, nowMS)
		return err
	})
	if err != nil {
		return fmt.Errorf("tts heartbeat: %w", err)
	}
	return nil
}

// RenewTTSStream extends the lease of the TTS service's open stream to untilMS, never shortening another's.
func (s *Store) RenewTTSStream(ctx context.Context, untilMS int64) error {
	err := s.db.unlocked(ctx, func(t *txn) error {
		_, err := t.ExecContext(ctx, `INSERT INTO tts_pod (id, pod, rooms, bots, states, beat_at_ms, stream_until_ms) VALUES (1, '', '[]', '[]', '[]', 0, ?)
			ON CONFLICT (id) DO UPDATE SET stream_until_ms = GREATEST(tts_pod.stream_until_ms, excluded.stream_until_ms)`, untilMS)
		return err
	})
	if err != nil {
		return fmt.Errorf("tts stream lease: %w", err)
	}
	return nil
}

// ReleaseTTSStream ends the stream lease if it still stands at untilMS, the ending stream's last
// renewal: another stream that renewed since keeps it.
func (s *Store) ReleaseTTSStream(ctx context.Context, untilMS int64) error {
	err := s.db.unlocked(ctx, func(t *txn) error {
		_, err := t.ExecContext(ctx, `UPDATE tts_pod SET stream_until_ms = 0 WHERE id = 1 AND stream_until_ms = ?`, untilMS)
		return err
	})
	if err != nil {
		return fmt.Errorf("tts stream release: %w", err)
	}
	return nil
}

// TTSPodState reads the TTS service's row; the zero TTSPod before any heartbeat or stream.
func (s *Store) TTSPodState(ctx context.Context) (TTSPod, error) {
	var p TTSPod
	var rooms, bots, states string
	err := s.db.QueryRowContext(ctx, `SELECT pod, rooms, bots, states, beat_at_ms, stream_until_ms FROM tts_pod WHERE id = 1`).
		Scan(&p.Pod, &rooms, &bots, &states, &p.BeatAtMS, &p.StreamUntilMS)
	if errors.Is(err, sql.ErrNoRows) {
		return TTSPod{}, nil
	}
	if err != nil {
		return TTSPod{}, fmt.Errorf("tts pod: %w", err)
	}
	if err := json.Unmarshal([]byte(rooms), &p.Rooms); err != nil {
		return TTSPod{}, fmt.Errorf("tts pod rooms: %w", err)
	}
	if err := json.Unmarshal([]byte(bots), &p.Bots); err != nil {
		return TTSPod{}, fmt.Errorf("tts pod bots: %w", err)
	}
	p.States = json.RawMessage(states)
	return p, nil
}

// SpeakingRooms counts the rooms among rooms with a persona that speaks and is not pending
// (013-AC34), in one query.
func (s *Store) SpeakingRooms(ctx context.Context, rooms []string) (int, error) {
	if len(rooms) == 0 {
		return 0, nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(DISTINCT room_id) FROM room_personas WHERE room_id = ANY(?) AND speaks = 1 AND pending = 0`,
		rooms).Scan(&n); err != nil {
		return 0, fmt.Errorf("speaking rooms: %w", err)
	}
	return n, nil
}

// DefaultVoices gives each speaking persona of room with no voice the one pick chooses from the
// voices the room uses, in one transaction: two pods picking at once cannot hand out one voice
// twice (013-AC16). It returns every persona's voice, by id.
func (s *Store) DefaultVoices(ctx context.Context, room string, pick func(used map[string]bool) string) (map[int64]string, error) {
	out := map[int64]string{}
	err := s.withTx(ctx, "default voices", func(tx *txn) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, voice, speaks FROM room_personas WHERE room_id = ? ORDER BY id`, room)
		if err != nil {
			return fmt.Errorf("default voices: %w", err)
		}
		var todo []int64
		used := map[string]bool{}
		for rows.Next() {
			var id int64
			var voice string
			var speaks bool
			if err := rows.Scan(&id, &voice, &speaks); err != nil {
				_ = rows.Close()
				return fmt.Errorf("default voices: %w", err)
			}
			out[id] = voice
			if voice != "" {
				used[voice] = true
			} else if speaks {
				todo = append(todo, id)
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("default voices: %w", err)
		}
		for _, id := range todo {
			v := pick(used)
			if _, err := tx.ExecContext(ctx, `UPDATE room_personas SET voice = ? WHERE id = ?`, v, id); err != nil {
				return fmt.Errorf("default voices: %w", err)
			}
			out[id], used[v] = v, true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
