package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	voicecat "github.com/babs/grill-room/internal/voices"
)

// MaxPronunciations caps the lexicon (013-AC31).
const MaxPronunciations = 1000

var (
	// ErrTermTaken is returned when the lexicon has the term already, whatever its case.
	ErrTermTaken = errors.New("the lexicon has that term already")
	// ErrLexiconFull is returned past MaxPronunciations entries.
	ErrLexiconFull = errors.New("the lexicon is full: delete a term first")
)

// Pronunciation is one lexicon entry; By is who last changed it, At when.
type Pronunciation struct {
	ID int64 `json:"id"`
	voicecat.Term
	By string `json:"by"`
	At string `json:"at"`
}

const pronunciationCols = `id, term, style, fr_en, fr_fr, en, by, at`

// Pronunciations is the lexicon, by term.
func (s *Store) Pronunciations(ctx context.Context) ([]Pronunciation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pronunciationCols+` FROM pronunciations ORDER BY term`)
	if err != nil {
		return nil, fmt.Errorf("pronunciations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Pronunciation{}
	for rows.Next() {
		var p Pronunciation
		if err := rows.Scan(&p.ID, &p.Term.Term, &p.Style, &p.FrEn, &p.FrFr, &p.En, &p.By, &p.At); err != nil {
			return nil, fmt.Errorf("pronunciations: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AddPronunciation adds t, changed by by. ErrTermTaken, ErrLexiconFull.
func (s *Store) AddPronunciation(ctx context.Context, t voicecat.Term, by string) (Pronunciation, error) {
	p := Pronunciation{Term: t, By: by, At: now()}
	err := s.withTx(ctx, "add pronunciation", func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pronunciations`).Scan(&n); err != nil {
			return err
		}
		if n >= MaxPronunciations {
			return ErrLexiconFull
		}
		if err := termFree(ctx, tx, t.Term, 0); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `INSERT INTO pronunciations (term, style, fr_en, fr_fr, en, by, at) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			t.Term, t.Style, t.FrEn, t.FrFr, t.En, by, p.At).Scan(&p.ID)
		if err != nil {
			return termErr(err)
		}
		return auditTx(ctx, tx, strconv.FormatInt(p.ID, 10), map[string]any{"entry": t})
	})
	return p, err
}

// UpdatePronunciation replaces entry id with t. ErrNotFound, ErrTermTaken.
func (s *Store) UpdatePronunciation(ctx context.Context, id int64, t voicecat.Term, by string) (Pronunciation, error) {
	p := Pronunciation{ID: id, Term: t, By: by, At: now()}
	err := s.withTx(ctx, "edit pronunciation", func(tx *sql.Tx) error {
		var old voicecat.Term
		err := tx.QueryRowContext(ctx, `SELECT term, style, fr_en, fr_fr, en FROM pronunciations WHERE id = ?`, id).
			Scan(&old.Term, &old.Style, &old.FrEn, &old.FrFr, &old.En)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := termFree(ctx, tx, t.Term, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE pronunciations SET term = ?, style = ?, fr_en = ?, fr_fr = ?, en = ?, by = ?, at = ? WHERE id = ?`,
			t.Term, t.Style, t.FrEn, t.FrFr, t.En, by, p.At, id); err != nil {
			return termErr(err)
		}
		return auditTx(ctx, tx, "", map[string]any{"from": old, "to": t})
	})
	return p, err
}

// DeletePronunciation removes entry id. ErrNotFound.
func (s *Store) DeletePronunciation(ctx context.Context, id int64) error {
	return s.withTx(ctx, "delete pronunciation", func(tx *sql.Tx) error {
		if err := auditTx(ctx, tx, "", nil); err != nil {
			return err
		}
		return execOne(ctx, tx, `DELETE FROM pronunciations WHERE id = ?`, id)
	})
}

// termFree is ErrTermTaken when another entry than id has term, ignoring case beyond ASCII, which
// the column's NOCASE does not fold ("mergé", "MERGÉ").
func termFree(ctx context.Context, tx *sql.Tx, term string, id int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT term FROM pronunciations WHERE id != ?`, id)
	if err != nil {
		return fmt.Errorf("pronunciation terms: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var other string
		if err := rows.Scan(&other); err != nil {
			return fmt.Errorf("pronunciation terms: %w", err)
		}
		if strings.EqualFold(other, term) {
			return ErrTermTaken
		}
	}
	return rows.Err()
}

func termErr(err error) error {
	if strings.Contains(err.Error(), "UNIQUE") {
		return ErrTermTaken
	}
	return fmt.Errorf("pronunciation: %w", err)
}

// seedPronunciations inserts Appendix A once, with its migration: later edits stand (013-AC32).
func seedPronunciations(ctx context.Context, tx *sql.Tx) error {
	for _, t := range voicecat.Seed() {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pronunciations (term, style, fr_en, fr_fr, en, by) VALUES (?, ?, ?, ?, ?, 'seed')`,
			t.Term, t.Style, t.FrEn, t.FrFr, t.En); err != nil {
			return fmt.Errorf("seed %q: %w", t.Term, err)
		}
	}
	return nil
}
