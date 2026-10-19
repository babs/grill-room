package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
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

// sessionTimeout bounds closing, pinging or unlocking a session held outside the caller's context.
const sessionTimeout = 5 * time.Second

// Advisory lock kinds: the first key of the two-key pg_advisory locks, one per cross-pod critical
// section. The two-key space never meets WriteLockKey's one-key space. Pods of two versions share
// them during a rolling update: a value never changes, a new kind takes a new one.
const (
	LockTurns      int32 = 1
	LockPlugin     int32 = 2
	LockRate       int32 = 3
	LockDaemon     int32 = 4
	LockPairing    int32 = 5
	LockPresence   int32 = 6
	LockSTTWorkers int32 = 7
	// LockLeader is the one leader lock, key 0 (Leader).
	LockLeader int32 = 8
)

// xactLock takes the transaction advisory lock (kind, key): writers of one key in unlocked
// transactions run one at a time.
func xactLock(ctx context.Context, t *txn, kind int32, key string) error {
	_, err := t.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?, hashtext(?))`, kind, key)
	return err
}

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
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionTimeout)
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
			ping, cancel := context.WithTimeout(ctx, sessionTimeout)
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

// LeaderApplication names the leader lock's session in pg_stat_activity, followed by the pod.
const LeaderApplication = "grill-room-leader"

// LeaderIdle is the idle_session_timeout of a leader session pinged every every: a leader gone
// without a FIN holds the lock until PostgreSQL drops its session, that long after its last ping.
func LeaderIdle(every time.Duration) time.Duration { return 3 * every }

// Leader campaigns for the cluster's one leader lock on a session of its own, until ctx ends: it
// tries the lock at once, then every interval, and holding it, pings the session as often. onChange learns
// each change: true once it holds the lock, false as soon as a ping or the session fails, or ctx
// ends. onErr learns why a session failed; the next try connects again.
func (s *Store) Leader(ctx context.Context, pod string, every time.Duration, onChange func(leader bool), onErr func(error)) {
	var conn *pgx.Conn
	leading := false
	drop := func() {
		if leading {
			leading = false
			onChange(false)
		}
		if conn != nil {
			// Closing the session frees the lock.
			c, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionTimeout)
			_ = conn.Close(c)
			cancel()
			conn = nil
		}
	}
	defer drop()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		c, cancel := context.WithTimeout(ctx, sessionTimeout)
		took, err := s.leaderTry(c, &conn, leading, pod, every)
		cancel()
		switch {
		case err != nil && ctx.Err() == nil:
			drop()
			onErr(fmt.Errorf("leader: %w", err))
		case took:
			leading = true
			onChange(true)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// leaderTry connects *conn when it has none, then pings it while leading, or tries the leader lock
// on it and reports whether it took it.
func (s *Store) leaderTry(ctx context.Context, conn **pgx.Conn, leading bool, pod string, every time.Duration) (took bool, err error) {
	if *conn == nil {
		cfg := s.db.cfg.Copy()
		cfg.RuntimeParams["application_name"] = LeaderApplication + " " + pod
		cfg.RuntimeParams["idle_session_timeout"] = strconv.FormatInt(LeaderIdle(every).Milliseconds(), 10)
		if *conn, err = pgx.ConnectConfig(ctx, cfg); err != nil {
			return false, err
		}
	}
	if leading {
		return false, (*conn).Ping(ctx)
	}
	err = (*conn).QueryRow(ctx, `SELECT pg_try_advisory_lock($1, 0)`, LockLeader).Scan(&took)
	return took, err
}

// LockRoom takes the session advisory lock (kind, room) on a connection it pins: a critical section
// of one room across pods that spans several transactions. With try it does not wait and reports
// ok false when another session holds it; without, it waits up to lock_timeout. release frees the
// lock and the connection. A caller that waits takes a pod-local guard per room first, so a pod
// pins at most one waiting connection per room; never call it holding the write lock, whose
// holder may be waiting for this one.
func (s *Store) LockRoom(ctx context.Context, kind int32, room string, try bool) (release func(), ok bool, err error) {
	// An exhausted pool would leave the caller, and the pod-local guard it holds, waiting unbounded.
	cctx, cancel := context.WithTimeout(ctx, s.db.lockTimeout)
	conn, err := s.db.db.Conn(cctx)
	cancel()
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
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionTimeout)
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

// SweepExpired deletes the shared state whose lease ended before nowMS, the status of the daemons
// left with no connection, and the rate events older than rateBeforeMS, the start of the longest
// limiter window. It returns the daemons whose last live connection it dropped: a pod that died
// left them, and nobody else learns they are gone. Every pod runs it: it is idempotent.
func (s *Store) SweepExpired(ctx context.Context, nowMS, rateBeforeMS int64) (gone []Daemon, err error) {
	if err := s.sweepPresence(ctx, nowMS); err != nil {
		return nil, fmt.Errorf("sweep expired: %w", err)
	}
	err = s.db.unlocked(ctx, func(t *txn) error {
		for _, table := range []string{"stt_workers", "pairings"} {
			//nolint:gosec // table names from the list above
			if _, err := t.ExecContext(ctx, `DELETE FROM `+table+` WHERE expires_at_ms <= ?`, nowMS); err != nil {
				return err
			}
		}
		// A daemon's status outlives its last connection only on a pod that died before removing it.
		if _, err := t.ExecContext(ctx, `DELETE FROM daemon_status st WHERE NOT EXISTS (SELECT 1 FROM daemon_connections c WHERE c.daemon_id = st.daemon_id)`); err != nil {
			return err
		}
		_, err := t.ExecContext(ctx, `DELETE FROM rate_events WHERE at_ms < ?`, rateBeforeMS)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("sweep expired: %w", err)
	}
	ids, err := s.expiredDaemons(ctx, nowMS)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		var d *Daemon
		err := s.db.unlocked(ctx, func(t *txn) error {
			if err := lockDaemon(ctx, t, id); err != nil {
				return err
			}
			res, err := t.ExecContext(ctx, `DELETE FROM daemon_connections WHERE daemon_id = ? AND expires_at_ms <= ?`, id, nowMS)
			if err != nil {
				return err
			}
			// Another pod's sweep, or the daemon's own disconnect, got there first.
			if n, err := res.RowsAffected(); err != nil || n == 0 {
				return err
			}
			if ok, err := daemonGone(ctx, t, id, nowMS); err != nil || !ok {
				return err
			}
			d = &Daemon{ID: id}
			err = t.QueryRowContext(ctx, `SELECT room_id, participant_id FROM daemons WHERE id = ?`, id).Scan(&d.RoomID, &d.ParticipantID)
			if errors.Is(err, sql.ErrNoRows) {
				d = nil
				return nil
			}
			return err
		})
		if err != nil {
			return gone, fmt.Errorf("sweep expired: %w", err)
		}
		if d != nil {
			gone = append(gone, *d)
		}
	}
	return gone, nil
}

// sweepBatch bounds the presence rows one sweep transaction deletes.
const sweepBatch = 1000

// sweepPresence deletes the presence rows expired at nowMS, a batch per transaction, locking them
// in primary key order (a person row before its tabs, as a presence write takes them): the sweep
// holds few locks at a time and seldom deadlocks with a write.
func (s *Store) sweepPresence(ctx context.Context, nowMS int64) error {
	for {
		var n int64
		err := s.db.unlocked(ctx, func(t *txn) error {
			res, err := t.ExecContext(ctx, `DELETE FROM presence WHERE (room_id, participant_id, tab) IN (SELECT room_id, participant_id, tab
				FROM presence WHERE expires_at_ms <= ? ORDER BY room_id, participant_id, tab LIMIT ? FOR UPDATE)`, nowMS, sweepBatch)
			if err != nil {
				return err
			}
			n, err = res.RowsAffected()
			return err
		})
		if err != nil || n < sweepBatch {
			return err
		}
	}
}

// expiredDaemons lists the daemons holding a connection whose lease ended before nowMS.
func (s *Store) expiredDaemons(ctx context.Context, nowMS int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT daemon_id FROM daemon_connections WHERE expires_at_ms <= ? ORDER BY daemon_id`, nowMS)
	if err != nil {
		return nil, fmt.Errorf("sweep expired: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("sweep expired: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sweep expired: %w", err)
	}
	return ids, nil
}
