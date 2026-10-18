package store

import (
	"context"
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ListenApplication names the LISTEN sessions in pg_stat_activity.
const ListenApplication = "grill-room-listen"

// listenPing is how long a LISTEN session waits for a notification before it checks the
// connection: a peer gone without a FIN would otherwise leave the pod deaf. Vars for tests, as is
// listenPinged, which learns each ping that passed.
var (
	listenPing   = 30 * time.Second
	listenPinged func()
)

// unlockSQL frees a LockRoom lock; a test swaps it for one that fails.
var unlockSQL = `SELECT pg_advisory_unlock($1, hashtext($2))`

// Notify sends payload on channel to every LISTEN session of the database, at once: it runs in no
// transaction and takes no lock, so callers send it after their commit.
func (s *Store) Notify(ctx context.Context, channel, payload string) error {
	if _, err := s.db.db.ExecContext(ctx, `SELECT pg_notify($1, $2)`, channel, payload); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	return nil
}

// Listen holds a connection of its own LISTENing on channel, calls ready once it listens and got
// for each notification, until ctx ends or the connection fails; it returns why.
func (s *Store) Listen(ctx context.Context, channel string, ready func(), got func(payload string)) error {
	cfg := s.db.cfg.Copy()
	cfg.RuntimeParams["application_name"] = ListenApplication
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = conn.Close(c)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	ready()
	for {
		wait, cancel := context.WithTimeout(ctx, listenPing)
		n, err := conn.WaitForNotification(wait)
		cancel()
		switch {
		case err == nil:
			got(n.Payload)
		case ctx.Err() != nil:
			return fmt.Errorf("listen: %w", ctx.Err())
		case pgconn.Timeout(err):
			ping, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := conn.Ping(ping)
			cancel()
			if err != nil {
				return fmt.Errorf("listen: %w", err)
			}
			if listenPinged != nil {
				listenPinged()
			}
		default:
			return fmt.Errorf("listen: %w", err)
		}
	}
}

// LockRoom takes the session advisory lock (kind, room) on a connection it pins: a critical section
// of one room across pods that spans several transactions. With try it does not wait and reports
// ok false when another session holds it; without, it waits up to lock_timeout. release frees the
// lock and the connection. Callers take a pod-local guard first, so pinned connections stay few;
// never call it holding the write lock, whose holder may be waiting for this one.
func (s *Store) LockRoom(ctx context.Context, kind int32, room string, try bool) (release func(), ok bool, err error) {
	conn, err := s.db.db.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("room lock: %w", err)
	}
	room = Sanitize(room)
	if try {
		err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1, hashtext($2))`, kind, room).Scan(&ok)
	} else {
		_, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock($1, hashtext($2))`, kind, room)
		ok = err == nil
	}
	if err != nil || !ok {
		_ = conn.Close()
		if err != nil {
			return nil, false, fmt.Errorf("room lock: %w", err)
		}
		return nil, false, nil
	}
	return func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var freed bool
		if err := conn.QueryRowContext(c, unlockSQL, kind, room).Scan(&freed); err != nil || !freed {
			// A session that may still hold the lock must not go back to the pool: closing it frees
			// the lock.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}, true, nil
}

// SweepExpired deletes the shared state whose lease ended before nowMS, and the rate events older
// than rateBeforeMS, the start of the longest limiter window. Every pod runs it: it is idempotent.
func (s *Store) SweepExpired(ctx context.Context, nowMS, rateBeforeMS int64) error {
	err := s.db.unlocked(ctx, func(t *txn) error {
		for _, table := range []string{"presence", "daemon_connections", "stt_workers", "pairings"} {
			//nolint:gosec // table names from the list above
			if _, err := t.ExecContext(ctx, `DELETE FROM `+table+` WHERE expires_at_ms <= ?`, nowMS); err != nil {
				return err
			}
		}
		_, err := t.ExecContext(ctx, `DELETE FROM rate_events WHERE at_ms < ?`, rateBeforeMS)
		return err
	})
	if err != nil {
		return fmt.Errorf("sweep expired: %w", err)
	}
	return nil
}
