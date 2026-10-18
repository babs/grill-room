package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// MaxSTTGlossary bounds a room's STT glossary, in runes.
const MaxSTTGlossary = 2000

// STTGlossary returns room's STT glossary (005-AC23).
func (s *Store) STTGlossary(ctx context.Context, roomID string) (string, error) {
	var g string
	err := s.db.QueryRowContext(ctx, `SELECT stt_glossary FROM rooms WHERE id = ?`, roomID).Scan(&g)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("stt glossary: %w", err)
	}
	return g, nil
}

// SetSTTGlossary sets room's STT glossary and says who did in a system line, in one transaction.
// An unchanged glossary changes nothing and returns a nil message. The caller broadcasts.
func (s *Store) SetSTTGlossary(ctx context.Context, roomID, glossary string, by *Participant) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "set stt glossary", func(tx *txn) error {
		var old string
		err := tx.QueryRowContext(ctx, `SELECT stt_glossary FROM rooms WHERE id = ?`, roomID).Scan(&old)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("set stt glossary: %w", err)
		}
		if old == glossary {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE rooms SET stt_glossary = ? WHERE id = ?`, glossary, roomID); err != nil {
			return fmt.Errorf("set stt glossary: %w", err)
		}
		m = &Message{RoomID: roomID, Kind: KindSystem, ParticipantID: by.ID, Sender: by.Name, Body: by.Name + " changed the transcription glossary"}
		return insertMessage(ctx, tx, m)
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}
