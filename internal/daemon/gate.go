package daemon

import (
	"slices"
	"sync"
	"time"
)

// DefaultMaxTurns is the host's global cap on concurrent turns until the page sets another
// (spec 003 decisions, grill Q5).
const DefaultMaxTurns = 2

// MaxTurnsLimit bounds the cap the page accepts: each turn is a sandbox and a Claude session.
const MaxTurnsLimit = 16

// gate holds the host's global cap on concurrent turns across every worker. Turns past it wait in
// one FIFO, whatever their instance and room (003-AC18).
type gate struct {
	mu      sync.Mutex
	max     int
	running map[slotKey]slotInfo
	queue   []waiter
	// granted counts the slots given; waited, those given after a wait, which took waitTotal in
	// all and waitMax at worst.
	granted, waited    int64
	waitTotal, waitMax time.Duration
}

// slotKey is one worker's slot: the instance, and the number the worker gave it.
type slotKey struct {
	instance string
	slot     int64
}

// slotInfo is what the page shows of a slot: its room and turn, and since when it holds or waits.
type slotInfo struct {
	room  string
	turn  int64
	since time.Time
}

type waiter struct {
	key  slotKey
	info slotInfo
	// send tells the worker its slot is granted.
	send func(Msg) error
}

func newGate(maxTurns int) *gate {
	return &gate{max: maxTurns, running: map[slotKey]slotInfo{}}
}

// acquire gives key a slot now, or queues it behind the others; send tells the worker which.
func (g *gate) acquire(key slotKey, room string, turn int64, send func(Msg) error) {
	g.mu.Lock()
	info := slotInfo{room: room, turn: turn, since: time.Now()}
	grant := len(g.running) < g.max && len(g.queue) == 0
	if grant {
		g.running[key] = info
		g.granted++
	} else {
		g.queue = append(g.queue, waiter{key: key, info: info, send: send})
	}
	g.mu.Unlock()
	reply := Msg{Type: "wait", Slot: key.slot}
	if grant {
		reply.Type = "grant"
	}
	if err := send(reply); err != nil && grant {
		// The worker is gone: its exit drops its slots anyway.
		g.release(key)
	}
}

// release frees key's slot, or takes it out of the queue, and grants the freed slots in order.
func (g *gate) release(key slotKey) {
	g.mu.Lock()
	delete(g.running, key)
	g.queue = slices.DeleteFunc(g.queue, func(w waiter) bool { return w.key == key })
	next := g.promoteLocked()
	g.mu.Unlock()
	g.notify(next)
}

// drop frees every slot of instance, whose worker exited.
func (g *gate) drop(instance string) {
	g.mu.Lock()
	for k := range g.running {
		if k.instance == instance {
			delete(g.running, k)
		}
	}
	g.queue = slices.DeleteFunc(g.queue, func(w waiter) bool { return w.key.instance == instance })
	next := g.promoteLocked()
	g.mu.Unlock()
	g.notify(next)
}

// setMax changes the cap; a higher one grants waiting turns at once. Running turns past a lower
// one finish.
func (g *gate) setMax(n int) {
	g.mu.Lock()
	g.max = n
	next := g.promoteLocked()
	g.mu.Unlock()
	g.notify(next)
}

// promoteLocked moves queued turns to free slots, in order, and returns them to notify.
func (g *gate) promoteLocked() []waiter {
	var out []waiter
	for len(g.running) < g.max && len(g.queue) > 0 {
		w := g.queue[0]
		g.queue = g.queue[1:]
		wait := time.Since(w.info.since)
		w.info.since = time.Now()
		g.running[w.key] = w.info
		g.granted++
		g.waited++
		g.waitTotal += wait
		g.waitMax = max(g.waitMax, wait)
		out = append(out, w)
	}
	return out
}

// notify sends the grants outside the lock: a worker slow to read must not hold the gate.
func (g *gate) notify(ws []waiter) {
	for _, w := range ws {
		if err := w.send(Msg{Type: "grant", Slot: w.key.slot}); err != nil {
			g.release(w.key)
		}
	}
}

// ConcurrencyView is the global cap as the page shows it, with the queue-wait figures to tune it.
type ConcurrencyView struct {
	MaxTurns int        `json:"max_turns"`
	Running  []SlotView `json:"running"`
	Queued   []SlotView `json:"queued"`
	// Granted counts the slots given since start; Waited, those that waited first, for
	// WaitTotalMS in all and WaitMaxMS at worst.
	Granted     int64 `json:"granted"`
	Waited      int64 `json:"waited"`
	WaitTotalMS int64 `json:"wait_total_ms"`
	WaitMaxMS   int64 `json:"wait_max_ms"`
}

// SlotView is a turn holding a slot or waiting for one, for SinceMS.
type SlotView struct {
	Instance string `json:"instance"`
	Room     string `json:"room"`
	Turn     int64  `json:"turn"`
	SinceMS  int64  `json:"since_ms"`
}

func (g *gate) view() ConcurrencyView {
	g.mu.Lock()
	defer g.mu.Unlock()
	v := ConcurrencyView{MaxTurns: g.max, Running: []SlotView{}, Queued: []SlotView{}, Granted: g.granted, Waited: g.waited,
		WaitTotalMS: g.waitTotal.Milliseconds(), WaitMaxMS: g.waitMax.Milliseconds()}
	slot := func(k slotKey, i slotInfo) SlotView {
		return SlotView{Instance: k.instance, Room: i.room, Turn: i.turn, SinceMS: time.Since(i.since).Milliseconds()}
	}
	for k, i := range g.running {
		v.Running = append(v.Running, slot(k, i))
	}
	slices.SortFunc(v.Running, func(a, b SlotView) int { return int(b.SinceMS - a.SinceMS) })
	for _, w := range g.queue {
		v.Queued = append(v.Queued, slot(w.key, w.info))
	}
	return v
}
