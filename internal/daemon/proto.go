package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/babs/grill-room/internal/sandbox"
)

// Proto is the supervisor-worker protocol version, docs/daemon-protocol.md. Peers up to ProtoSkew
// apart talk at the lower of their two versions, so every change within that window must stay
// additive: new fields and new message types only, and unknown ones are ignored.
//
// Version 2 added the config's access and the access request: the accounts' proxies (003-AC14).
// Version 3 added the slots under the host's global cap: acquire, wait, grant, release (003-AC18).
// Version 4 added the room plugins' requests, plugin_options and plugin_fetch (009-AC19, 009-AC22).
// Version 5 added remove_room (003b-AC6).
// Version 6 added egress_switches, room_allowlist_remove and the denied reply's room_hits (014).
const (
	Proto     = 6
	ProtoSkew = 1
)

// protoEgress is the first version whose workers take egress_switches and room_allowlist_remove.
const protoEgress = 6

// protoSlots is the first version whose workers ask for a slot before each turn.
const protoSlots = 3

// protoPlugins is the first version whose workers take plugin_options and plugin_fetch.
const protoPlugins = 4

// protoRemove is the first version whose workers take remove_room.
const protoRemove = 5

// Negotiate returns the version a peer at own talks with a peer at peer, or why they cannot.
func Negotiate(own, peer int) (int, error) {
	if peer < own-ProtoSkew || peer > own+ProtoSkew {
		return 0, fmt.Errorf("protocol version %d is outside the supported range %d to %d: run a supervisor and a worker from releases closer in time",
			peer, own-ProtoSkew, own+ProtoSkew)
	}
	return min(own, peer), nil
}

// Msg is one line of the protocol. Type says which other fields are set.
type Msg struct {
	Type string `json:"type"`
	// ID numbers a supervisor request; the worker's reply carries it back.
	ID int64 `json:"id,omitempty"`
	// Slot numbers a worker's acquire; the supervisor's wait and grant, and the worker's
	// release, carry it back.
	Slot int64 `json:"slot,omitempty"`
	// Proto is the sender's own version, in hello and config.
	Proto    int    `json:"proto,omitempty"`
	Version  string `json:"version,omitempty"`
	Instance string `json:"instance,omitempty"`
	Error    string `json:"error,omitempty"`

	Config    *WorkerConfig    `json:"config,omitempty"`
	Room      string           `json:"room,omitempty"`
	Settings  *Settings        `json:"settings,omitempty"`
	Allowlist []string         `json:"allowlist,omitempty"`
	N         int              `json:"n,omitempty"`
	Status    *Status          `json:"status,omitempty"`
	Denied    []sandbox.Denied `json:"denied,omitempty"`
	// Hits, in a denied reply, counts the egress requests let through, by host (003b-AC16), and
	// RoomHits by room link too (014-AC3).
	Hits     map[string]int            `json:"hits,omitempty"`
	RoomHits map[string]map[string]int `json:"room_hits,omitempty"`
	// Switches is an egress_switches request (014-AC1, 014-AC26); Domain, with Room, a
	// room_allowlist_remove one (014-AC15).
	Switches *EgressSwitches `json:"switches,omitempty"`
	Domain   string          `json:"domain,omitempty"`
	Metrics  *Metrics        `json:"metrics,omitempty"`
	Turn     *TurnEvent      `json:"turn,omitempty"`
	Log      json.RawMessage `json:"log,omitempty"`
	Access   *Access         `json:"access,omitempty"`
	// Plugin, Values and Clear are a plugin_options request: the values the host gave the options
	// of the room's plugin id Plugin in Room, and the options to forget. Fetch and Accept are a
	// plugin_fetch request.
	Plugin int64          `json:"plugin,omitempty"`
	Values map[string]any `json:"values,omitempty"`
	Clear  []string       `json:"clear,omitempty"`
	Fetch  int64          `json:"fetch,omitempty"`
	Accept bool           `json:"accept,omitempty"`
	// Force, in remove_room, removes the room here even when its server does not forget this daemon.
	Force bool `json:"force,omitempty"`
}

// Access is how a worker's rooms reach Claude: the bearer-swap proxy of each room's account, which
// runs in the supervisor. A worker never sees a Claude token (003-AC14).
type Access struct {
	// Proxies are keyed by room link.
	Proxies map[string]Proxy `json:"proxies,omitempty"`
	// CutOff is set while the host has cut this worker off: its turns end and no new one starts (003-AC15).
	CutOff bool `json:"cut_off,omitempty"`
}

// Proxy is one room's way to its account's proxy.
type Proxy struct {
	// Addr is what the sandbox relay dials, "<gateway>:<port>".
	Addr string `json:"addr,omitempty"`
	// Bearer is the dummy token the proxy knows this worker by.
	Bearer string `json:"bearer,omitempty"`
	// Error says, in words fit for the room, why the room cannot reach Claude now. It never names
	// the account (003-AC17).
	Error string `json:"error,omitempty"`
}

// WorkerConfig is everything a worker runs with; the supervisor sends it after the handshake.
type WorkerConfig struct {
	Instance string `json:"instance"`
	// ConfigDir, StateDir and CacheDir are the instance's own directories.
	ConfigDir string `json:"config_dir"`
	StateDir  string `json:"state_dir"`
	CacheDir  string `json:"cache_dir"`
	// SandboxRoot keys the sandbox names, so that two supervisors never share a sandbox.
	SandboxRoot string `json:"sandbox_root"`
	LogLevel    string `json:"log_level"`
	Image       string `json:"image,omitempty"`
	// Settings apply until the page saves the instance's own.
	Settings Settings       `json:"settings"`
	Limits   sandbox.Limits `json:"limits"`
	CacheMB  int64          `json:"cache_mb"`
	// Rooms are the instance's rooms, oldest first. The worker reconnects to the newest one that
	// holds a token, unless Pair names a room to pair with now.
	Rooms []string `json:"rooms,omitempty"`
	Pair  string   `json:"pair,omitempty"`
	// Access is the rooms' proxies at start; the access request replaces it.
	Access *Access `json:"access,omitempty"`
	// agreed is the protocol version the handshake settled on.
	agreed int
}

// Metrics is the worker process's health, sent every MetricsEvery.
type Metrics struct {
	UptimeSeconds int64  `json:"uptime_seconds"`
	Turns         int64  `json:"turns"`
	FailedTurns   int64  `json:"failed_turns"`
	HeapBytes     uint64 `json:"heap_bytes"`
	Goroutines    int    `json:"goroutines"`
}

// TurnEvent is a turn starting or ending in one of the worker's rooms.
type TurnEvent struct {
	Room string `json:"room"`
	ID   int64  `json:"id"`
	// Phase is "start" or "end".
	Phase      string `json:"phase"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	// QueueMS is how long the turn waited for a slot under the host's global cap (003-AC18).
	QueueMS int64 `json:"queue_ms,omitempty"`
	// Persona, ContextTokens and ContextWindow come with a turn's end: the session's context use,
	// 0 when unknown (003-AC24).
	Persona       string `json:"persona,omitempty"`
	ContextTokens int64  `json:"context_tokens,omitempty"`
	ContextWindow int64  `json:"context_window,omitempty"`
	// Also at the end, for the supervisor's monitoring (003-AC27): why the turn ran, the model and
	// effort the persona asked for and those it ran with, its tokens summed over every model
	// claude called, claude's cost estimate (nil when unknown) and the workspace tar's size.
	// ModelNote and ModelMismatch qualify Model as turn.Result does (005-AC10).
	Cause            string   `json:"cause,omitempty"`
	AskedModel       string   `json:"asked_model,omitempty"`
	AskedEffort      string   `json:"asked_effort,omitempty"`
	Model            string   `json:"model,omitempty"`
	ModelNote        string   `json:"model_note,omitempty"`
	ModelMismatch    bool     `json:"model_mismatch,omitempty"`
	Effort           string   `json:"effort,omitempty"`
	TokensIn         int64    `json:"tokens_in,omitempty"`
	TokensOut        int64    `json:"tokens_out,omitempty"`
	TokensCacheRead  int64    `json:"tokens_cache_read,omitempty"`
	TokensCacheWrite int64    `json:"tokens_cache_write,omitempty"`
	CostUSD          *float64 `json:"cost_usd,omitempty"`
	WorkspaceBytes   int64    `json:"workspace_bytes,omitempty"`
}

// maxLine bounds one protocol line: a status with a long allowlist, or a full denied list.
const maxLine = 4 << 20

// Conn reads and writes protocol lines. Send is safe from several goroutines; Recv is not.
type Conn struct {
	sc  *bufio.Scanner
	wmu sync.Mutex
	w   io.Writer
}

// NewConn talks over r and w: a worker's stdin and stdout, or the supervisor's pipes to them.
func NewConn(r io.Reader, w io.Writer) *Conn {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	return &Conn{sc: sc, w: w}
}

// Send writes m as one line.
func (c *Conn) Send(m Msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode %s: %w", m.Type, err)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}

// Recv reads the next message; io.EOF once the peer closed its end.
func (c *Conn) Recv() (Msg, error) {
	if !c.sc.Scan() {
		if err := c.sc.Err(); err != nil {
			return Msg{}, err
		}
		return Msg{}, io.EOF
	}
	var m Msg
	if err := json.Unmarshal(c.sc.Bytes(), &m); err != nil {
		return Msg{}, fmt.Errorf("bad protocol line: %w", err)
	}
	return m, nil
}
