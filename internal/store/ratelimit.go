package store

import (
	"context"
	"errors"
	"fmt"
)

// ErrRateFull refuses a hit while its scope holds scopeCap events in the window: a flood of
// distinct keys cannot grow the table without bound.
var ErrRateFull = errors.New("rate limit table full")

func rateCount(ctx context.Context, q rowQuerier, scope, keyHash string, sinceMS int64) (n int, oldestMS int64, err error) {
	err = q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(at_ms), 0) FROM rate_events
		WHERE scope = ? AND key_hash = ? AND at_ms > ?`, scope, keyHash, sinceMS).Scan(&n, &oldestMS)
	return n, oldestMS, err
}

// RateHit records an event for keyHash in scope unless maxHits events are already after sinceMS,
// the window's start. It returns the new event's id, or 0 and the oldest counted event's time;
// ErrRateFull when scope holds scopeCap events after sinceMS. A key over its cap is refused by a
// plain read; otherwise the count and the insert hold the key's advisory lock, never the write
// lock, so concurrent requests on any pod cannot both pass and a flood stalls no other write.
func (s *Store) RateHit(ctx context.Context, scope, keyHash string, maxHits, scopeCap int, sinceMS, nowMS int64) (id, oldestMS int64, err error) {
	n, oldestMS, err := rateCount(ctx, s.db, scope, keyHash, sinceMS)
	if err != nil {
		return 0, 0, fmt.Errorf("rate hit: %w", err)
	}
	if n >= maxHits {
		return 0, oldestMS, nil
	}
	err = s.db.unlocked(ctx, func(t *txn) error {
		if err := xactLock(ctx, t, LockRate, scope+" "+keyHash); err != nil {
			return err
		}
		if n, oldestMS, err = rateCount(ctx, t, scope, keyHash, sinceMS); err != nil || n >= maxHits {
			return err
		}
		var live int
		if err := t.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM rate_events WHERE scope = ? AND at_ms > ? LIMIT ?) e`,
			scope, sinceMS, scopeCap).Scan(&live); err != nil {
			return err
		}
		if live >= scopeCap {
			return ErrRateFull
		}
		return t.QueryRowContext(ctx, `INSERT INTO rate_events(scope, key_hash, at_ms) VALUES(?,?,?) RETURNING id`, scope, keyHash, nowMS).Scan(&id)
	})
	if err != nil {
		return 0, 0, fmt.Errorf("rate hit: %w", err)
	}
	return id, oldestMS, nil
}

// RateUnhit deletes the event id, as if it never happened.
func (s *Store) RateUnhit(ctx context.Context, id int64) error {
	if err := s.db.unlocked(ctx, func(t *txn) error {
		_, err := t.ExecContext(ctx, `DELETE FROM rate_events WHERE id = ?`, id)
		return err
	}); err != nil {
		return fmt.Errorf("rate unhit: %w", err)
	}
	return nil
}

// RateReset deletes every event of keyHash in scope.
func (s *Store) RateReset(ctx context.Context, scope, keyHash string) error {
	if err := s.db.unlocked(ctx, func(t *txn) error {
		_, err := t.ExecContext(ctx, `DELETE FROM rate_events WHERE scope = ? AND key_hash = ?`, scope, keyHash)
		return err
	}); err != nil {
		return fmt.Errorf("rate reset: %w", err)
	}
	return nil
}
