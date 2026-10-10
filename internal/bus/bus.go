// Package bus is the message bus: persistent store plus live fan-out.
//
// Derived from github.com/babs/airc pkg/bus at commit b7afc63.
package bus

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/babs/grill-room/internal/store"
)

// Bus posts messages to the store and wakes every live subscriber.
//
// Subscribers receive only the id of the room that changed: a new message, a revoked invite or a
// deleted room. They re-read the store, so a dropped signal never loses data.
type Bus struct {
	Store *store.Store

	mu   sync.Mutex
	subs map[chan string]struct{}
	// dropped counts signals a full subscriber missed; exported as a metric.
	dropped atomic.Uint64
}

// New wraps a store.
func New(s *store.Store) *Bus {
	return &Bus{Store: s, subs: map[chan string]struct{}{}}
}

// Post persists m then wakes subscribers. m.ID and m.TS are filled in.
func (b *Bus) Post(ctx context.Context, m *store.Message) error {
	if err := b.Store.InsertMessage(ctx, m); err != nil {
		return err
	}
	b.Notify(m.RoomID)
	return nil
}

// Notify wakes subscribers about a change in room.
func (b *Bus) Notify(room string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- room:
		default:
			// Slow subscriber: drop. SSE handlers re-read the store on their heartbeat.
			b.dropped.Add(1)
		}
	}
}

// Dropped returns how many signals were lost to full subscriber buffers.
func (b *Bus) Dropped() uint64 { return b.dropped.Load() }

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
