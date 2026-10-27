package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/babs/grill-room/internal/render"
)

// voiceLookback bounds the lines of a participant the repeat check reads: far more than one
// speaker says within a dedupe window.
const voiceLookback = 50

// VoiceStored is what PostVoiceLine did with a line.
type VoiceStored int

const (
	// VoiceDeduped dropped the line as the other source's repeat (010-AC16).
	VoiceDeduped VoiceStored = iota
	// VoiceInserted stored the line as a new message.
	VoiceInserted
	// VoiceAppended joined the line to its speaker's open paragraph (018-AC4).
	VoiceAppended
)

// VoiceJoin is what decides whether a line joins its speaker's open paragraph (018-AC4).
type VoiceJoin struct {
	// GapS is the pause before the line in seconds, nil when the client did not say (018-AC8).
	GapS *float64
	// MaxGap is the pause that ends a paragraph; 0 never joins.
	MaxGap time.Duration
	// MaxChars bounds the joined body, in characters.
	MaxChars int
}

func (j VoiceJoin) wanted() bool { return j.GapS != nil && *j.GapS < j.MaxGap.Seconds() }

// PostVoiceLine stores m, a voice line at now, unless dup reports it repeats one of the bodies of
// its participant's voice lines from the other source since now-window (010-AC16). When j allows
// it, the line joins the room's latest non-typed message instead, its speaker's open paragraph
// (018-AC4, 020-AC1).
// The check and the write are one write transaction: two pods posting the same utterance store it
// once. m comes back as stored: the whole paragraph after an append.
// ponytail: the check and the insert hold the cluster-wide write lock; take a per-participant
// advisory lock instead if voice lines ever queue behind other writes.
func (s *Store) PostVoiceLine(ctx context.Context, m *Message, now time.Time, window time.Duration, j VoiceJoin, dup func(other []string) bool) (VoiceStored, error) {
	res := VoiceDeduped
	err := s.withTx(ctx, "post voice line", func(t *txn) error {
		// A paragraph keeps its first line's ts: its last append is what the window measures (018-AC6).
		rows, err := t.QueryContext(ctx, `SELECT body FROM (SELECT body, kind, source, COALESCE(appended_at, ts) AS at FROM messages
			WHERE participant_id = ? ORDER BY id DESC LIMIT ?) m
			WHERE kind = 'voice' AND source <> ? AND at >= ?`, m.ParticipantID, voiceLookback, m.Source, formatTS(now.Add(-window)))
		if err != nil {
			return fmt.Errorf("post voice line: %w", err)
		}
		var other []string
		for rows.Next() {
			var b string
			if err := rows.Scan(&b); err != nil {
				_ = rows.Close()
				return fmt.Errorf("post voice line: %w", err)
			}
			other = append(other, b)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("post voice line: %w", err)
		}
		if len(other) > 0 && dup(other) {
			return nil
		}
		if j.wanted() {
			joined, err := appendVoiceLine(ctx, t, m, now, j.MaxChars)
			if err != nil {
				return fmt.Errorf("post voice line: %w", err)
			}
			if joined {
				res = VoiceAppended
				return nil
			}
		}
		if err := insertMessage(ctx, t, m); err != nil {
			return fmt.Errorf("post voice line: %w", err)
		}
		// The window reads the caller's clock, which tests move: the line carries that time.
		m.TS = now.UTC()
		if _, err := t.ExecContext(ctx, `UPDATE messages SET ts = ? WHERE id = ?`, formatTS(m.TS), m.ID); err != nil {
			return fmt.Errorf("post voice line: %w", err)
		}
		res = VoiceInserted
		return nil
	})
	if err != nil {
		return VoiceDeduped, err
	}
	return res, nil
}

// appendVoiceLine joins m to the room's latest message other than a typed one when that is m's
// speaker's open paragraph (018-AC4, 020-AC1), and reports whether it did; m then holds the
// paragraph. Like an edit it bumps change_seq, so every page redraws it, but it writes no version
// and no edited_at (018-AC5).
func appendVoiceLine(ctx context.Context, t *txn, m *Message, now time.Time, maxChars int) (bool, error) {
	var id int64
	var pid sql.NullInt64
	var kind, body string
	var source, edited, deleted sql.NullString
	err := t.QueryRowContext(ctx, `SELECT id, participant_id, kind, source, body, edited_at, deleted_at FROM messages
		WHERE room_id = ? AND kind <> 'text' ORDER BY id DESC LIMIT 1`, m.RoomID).Scan(&id, &pid, &kind, &source, &body, &edited, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	joined := body + " " + m.Body
	if kind != KindVoice || pid.Int64 != m.ParticipantID || source.String != m.Source || edited.Valid || deleted.Valid ||
		utf8.RuneCountInString(joined) > maxChars {
		return false, nil
	}
	// A turn that read the paragraph would never see its tail: MessageChanges reports changes
	// through versions, and an append writes none. Turns start under the room's turn lock, so the
	// latest one has the highest cursor; one being prepared holds that lock and has no row yet. A
	// typed message after the paragraph that started a turn puts the cursor past it (020-AC3).
	var read, free bool
	if err := t.QueryRowContext(ctx, `SELECT COALESCE((SELECT message_cursor FROM turns WHERE room_id = ? ORDER BY id DESC LIMIT 1), 0) >= ?,
		pg_try_advisory_xact_lock_shared(?, hashtext(?))`, m.RoomID, id, LockTurns, Sanitize(m.RoomID)).Scan(&read, &free); err != nil {
		return false, err
	}
	if read || !free {
		return false, nil
	}
	html := render.MessageHTML(joined)
	if _, err := t.ExecContext(ctx, `UPDATE messages SET body = ?, html = ?, appended_at = ?,
		change_seq = (SELECT COALESCE(MAX(change_seq), 0) + 1 FROM messages WHERE room_id = ?) WHERE id = ?`,
		joined, html, formatTS(now), m.RoomID, id); err != nil {
		return false, err
	}
	p, err := scanMessage(t.QueryRowContext(ctx, messageCols+`WHERE id = ?`, id))
	if err != nil {
		return false, err
	}
	*m = *p
	return true, nil
}
