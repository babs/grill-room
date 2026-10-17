package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SetSpeech changes the room's call settings and says so in the chat; nil when nothing changed
// (013-AC25). The caller checks sp.Valid.
func (s *Store) SetSpeech(ctx context.Context, roomID string, sp Speech, by *Participant) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "set speech", func(tx *sql.Tx) error {
		var old Speech
		err := tx.QueryRowContext(ctx, `SELECT voice_quiet_ms, voice_resume_s, voice_queue_s, voice_words FROM rooms WHERE id = ?`, roomID).
			Scan(&old.QuietMS, &old.ResumeS, &old.QueueS, &old.Words)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var changes []string
		add := func(label string, from, to int, unit string) {
			if from != to {
				changes = append(changes, fmt.Sprintf("%s %d%s → %d%s", label, from, unit, to, unit))
			}
		}
		add("quiet before speaking", old.QuietMS, sp.QuietMS, " ms")
		add("resume window", old.ResumeS, sp.ResumeS, " s")
		add("longest wait", old.QueueS, sp.QueueS, " s")
		add("spoken words", old.Words, sp.Words, "")
		if len(changes) == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE rooms SET voice_quiet_ms = ?, voice_resume_s = ?, voice_queue_s = ?, voice_words = ? WHERE id = ?`,
			sp.QuietMS, sp.ResumeS, sp.QueueS, sp.Words, roomID); err != nil {
			return err
		}
		m = &Message{RoomID: roomID, Kind: KindSystem, Sender: "admin", Body: "The admin changed the call voices: " + strings.Join(changes, ", ") + "."}
		if by != nil {
			m.ParticipantID, m.Sender = by.ID, by.Name
			m.Body = by.Name + " changed the call voices: " + strings.Join(changes, ", ") + "."
		}
		if err := insertMessage(ctx, tx, m); err != nil {
			return err
		}
		return auditTx(ctx, tx, "", map[string]any{"from": old, "to": sp})
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

// Sample is a voice's sample in one language (013-AC27).
type Sample struct {
	Voice, Lang, SHA256 string
	Data                []byte
}

// SampleHashes lists the samples kept, "voice/lang" to sha256.
func (s *Store) SampleHashes(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT voice, lang, sha256 FROM tts_samples`)
	if err != nil {
		return nil, fmt.Errorf("sample hashes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var v, l, h string
		if err := rows.Scan(&v, &l, &h); err != nil {
			return nil, fmt.Errorf("sample hashes: %w", err)
		}
		out[v+"/"+l] = h
	}
	return out, rows.Err()
}

// PutSample keeps a voice's sample, replacing the one before.
func (s *Store) PutSample(ctx context.Context, x Sample) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tts_samples(voice, lang, sha256, data) VALUES(?,?,?,?)
		ON CONFLICT(voice, lang) DO UPDATE SET sha256 = excluded.sha256, data = excluded.data`, x.Voice, x.Lang, x.SHA256, x.Data)
	if err != nil {
		return fmt.Errorf("put sample: %w", err)
	}
	return nil
}

// GetSample returns a voice's sample; ErrNotFound when the pod never handed it over.
func (s *Store) GetSample(ctx context.Context, voice, lang string) (*Sample, error) {
	x := Sample{Voice: voice, Lang: lang}
	err := s.db.QueryRowContext(ctx, `SELECT sha256, data FROM tts_samples WHERE voice = ? AND lang = ?`, voice, lang).Scan(&x.SHA256, &x.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get sample: %w", err)
	}
	return &x, nil
}
