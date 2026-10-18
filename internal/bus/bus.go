// Package bus is the message bus: persistent store plus live fan-out.
//
// Derived from github.com/babs/airc pkg/bus at commit b7afc63.
package bus

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/babs/grill-room/internal/store"
)

// All is the change every subscriber takes as its own: after a LISTEN (re)connects, a pod cannot
// know what it missed, so every stream re-reads the store.
const All = "*"

// channel is the NOTIFY channel the pods share.
const channel = "grill_room"

// notifyTimeout bounds one pg_notify; a lost notify only delays readers to their heartbeat.
const notifyTimeout = 5 * time.Second

// Bus posts messages to the store and wakes every live subscriber, on this pod and, once Run
// runs, on every pod of the database.
//
// Subscribers receive only the id of the room that changed: a new message, a revoked invite or a
// deleted room, or All. They re-read the store, so a dropped signal never loses data.
type Bus struct {
	Store *store.Store
	// OnListenError, when set before Run, learns why each LISTEN connection ended.
	OnListenError func(error)

	// pod is the replica's identity: it tells this bus's notifications apart from other pods', so
	// it drops its own echoes.
	pod string

	mu   sync.Mutex
	subs map[chan string]struct{}
	// pending holds the ids Run's sender has yet to notify; nil until Run starts.
	pending map[string]struct{}
	kick    chan struct{}
	// dropped counts signals a full subscriber missed; exported as a metric.
	dropped                                atomic.Uint64
	sent, received, reconnects, listenErrs atomic.Uint64
}

// New wraps a store for the replica named pod, unique among the replicas sharing the database and
// free of spaces.
func New(s *store.Store, pod string) *Bus {
	return &Bus{Store: s, pod: pod, subs: map[chan string]struct{}{}, kick: make(chan struct{}, 1)}
}

// Pod is the replica's identity New was given.
func (b *Bus) Pod() string { return b.pod }

// Post persists m then wakes subscribers. m.ID and m.TS are filled in.
func (b *Bus) Post(ctx context.Context, m *store.Message) error {
	if err := b.Store.InsertMessage(ctx, m); err != nil {
		return err
	}
	b.Notify(m.RoomID)
	return nil
}

// Notify wakes subscribers about a change in room, here at once and on the other pods through Run.
// It never blocks: Run's sender coalesces the ids and sends them in its own goroutine.
func (b *Bus) Notify(room string) {
	b.mu.Lock()
	b.wakeLocked(room)
	queued := b.pending != nil
	if queued {
		b.pending[room] = struct{}{}
	}
	b.mu.Unlock()
	if queued {
		select {
		case b.kick <- struct{}{}:
		default:
		}
	}
}

func (b *Bus) wake(room string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.wakeLocked(room)
}

func (b *Bus) wakeLocked(room string) {
	for ch := range b.subs {
		select {
		case ch <- room:
		default:
			// Slow subscriber: drop. SSE handlers re-read the store on their heartbeat.
			b.dropped.Add(1)
		}
	}
}

// Run carries notifications between the pods until ctx ends: it sends this pod's, and listens for
// the others', reconnecting with a backoff of 1 s to 5 s. Each LISTEN that starts wakes every
// subscriber with All. It returns nil.
func (b *Bus) Run(ctx context.Context) error {
	b.mu.Lock()
	b.pending = map[string]struct{}{}
	b.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.send(ctx)
	}()
	defer func() {
		<-done
		b.mu.Lock()
		b.pending = nil
		b.mu.Unlock()
	}()
	backoff, first := time.Second, true
	for {
		err := b.Store.Listen(ctx, channel, func() {
			if !first {
				b.reconnects.Add(1)
			}
			first, backoff = false, time.Second
			b.wake(All)
		}, b.got)
		if ctx.Err() != nil {
			return nil
		}
		b.listenErrs.Add(1)
		if b.OnListenError != nil {
			b.OnListenError(err)
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		backoff = min(2*backoff, 5*time.Second)
	}
}

// send notifies the other pods of the pending ids until ctx ends.
func (b *Bus) send(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.kick:
		}
		b.mu.Lock()
		ids := b.pending
		b.pending = make(map[string]struct{}, len(ids))
		b.mu.Unlock()
		for id := range ids {
			c, cancel := context.WithTimeout(ctx, notifyTimeout)
			if b.Store.Notify(c, channel, b.pod+" "+id) == nil {
				b.sent.Add(1)
			}
			cancel()
		}
	}
}

// got wakes the subscribers for another pod's notification "<pod> <id>".
func (b *Bus) got(payload string) {
	pod, id, ok := strings.Cut(payload, " ")
	if !ok || pod == b.pod {
		return
	}
	b.received.Add(1)
	b.wake(id)
}

// Dropped returns how many signals were lost to full subscriber buffers.
func (b *Bus) Dropped() uint64 { return b.dropped.Load() }

// NotifySent returns how many notifications this pod sent to the others (017-AC20).
func (b *Bus) NotifySent() uint64 { return b.sent.Load() }

// NotifyReceived returns how many notifications this pod got from the others (017-AC20).
func (b *Bus) NotifyReceived() uint64 { return b.received.Load() }

// ListenReconnects returns how many times the LISTEN connection started again after the first
// (017-AC20).
func (b *Bus) ListenReconnects() uint64 { return b.reconnects.Load() }

// ListenErrors returns how many LISTEN connections failed or ended while Run ran.
func (b *Bus) ListenErrors() uint64 { return b.listenErrs.Load() }

// Subscribe returns a channel of room ids and a cancel func. buffer sizes the channel.
func (b *Bus) Subscribe(buffer int) (<-chan string, func()) {
	ch := make(chan string, buffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}
