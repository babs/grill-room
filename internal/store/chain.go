package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// DefaultChainBudget is a new room's chain budget (spec 002 decisions).
const DefaultChainBudget = 25

// MaxChainBudget bounds the budget a member sets: past it, "suspend limit" says what is meant.
const MaxChainBudget = 1000

// ErrChainPaused is returned for a chain link past the budget: the turn is dropped, not queued.
var ErrChainPaused = errors.New("the agent chain reached its budget")

// Chain counts the agent-triggered turns of a room since a human last took the lead (002-AC25).
type Chain struct {
	Budget int `json:"budget"`
	Count  int `json:"count"`
	// Suspended lifts the budget: links always run, and still count.
	Suspended bool `json:"suspended"`
}

// ChainEvent is what moves the chain counter.
type ChainEvent string

// Links are agent-triggered turns; resets come from a human (002-AC26). Voice lines and plain
// text are no event at all.
const (
	LinkEveryN   ChainEvent = "every_n"
	LinkCall     ChainEvent = "call"
	ResetButton  ChainEvent = "button"
	ResetRearm   ChainEvent = "rearm"
	ResetMention ChainEvent = "mention"
	Suspend      ChainEvent = "suspend"
	Unsuspend    ChainEvent = "unsuspend"
)

// Paused reports the budget reached with the limit on: the next link is dropped.
func (c Chain) Paused() bool { return !c.Suspended && c.Count >= c.Budget }

// On returns the chain after e, and for a link whether it runs. A paused chain drops the link and
// stays as it is.
func (c Chain) On(e ChainEvent) (Chain, bool) {
	switch e {
	case LinkEveryN, LinkCall:
		if c.Paused() {
			return c, false
		}
		c.Count++
	case ResetButton, ResetRearm, ResetMention:
		c.Count = 0
	case Suspend:
		c.Suspended = true
	case Unsuspend:
		c.Suspended = false
	}
	return c, true
}

func roomChain(ctx context.Context, q querier, roomID string) (Chain, error) {
	var c Chain
	err := q.QueryRowContext(ctx, `SELECT chain_budget, chain_count, chain_suspended FROM rooms WHERE id = ?`, roomID).Scan(&c.Budget, &c.Count, &c.Suspended)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, fmt.Errorf("room chain: %w", err)
	}
	return c, nil
}

func saveChain(ctx context.Context, tx *sql.Tx, roomID string, c Chain) error {
	if _, err := tx.ExecContext(ctx, `UPDATE rooms SET chain_budget = ?, chain_count = ?, chain_suspended = ? WHERE id = ?`,
		c.Budget, c.Count, c.Suspended, roomID); err != nil {
		return fmt.Errorf("save chain: %w", err)
	}
	return nil
}

// chainOn applies e to the room's chain in tx; false for a dropped link, which changes nothing.
func chainOn(ctx context.Context, tx *sql.Tx, roomID string, e ChainEvent) (Chain, bool, error) {
	c, err := roomChain(ctx, tx, roomID)
	if err != nil {
		return c, false, err
	}
	next, ok := c.On(e)
	if !ok || next == c {
		return next, ok, nil
	}
	return next, true, saveChain(ctx, tx, roomID, next)
}

// ResetChain is a human taking the lead: a persona button or a message mentioning a persona.
func (s *Store) ResetChain(ctx context.Context, roomID string, e ChainEvent) error {
	return s.withTx(ctx, "reset chain", func(tx *sql.Tx) error {
		_, _, err := chainOn(ctx, tx, roomID, e)
		return err
	})
}

// ChainControl is a member's change to the chain from its banner or the personas panel; nil and
// false fields change nothing.
type ChainControl struct {
	Budget    *int
	Rearm     bool
	Suspended *bool
}

// ControlChain applies c to the room's chain and records each change in the chat, signed by.
// It returns the chain and the last line written, nil when nothing changed.
func (s *Store) ControlChain(ctx context.Context, roomID string, c ChainControl, by *Participant) (Chain, *Message, error) {
	var ch Chain
	var m *Message
	err := s.withTx(ctx, "control chain", func(tx *sql.Tx) error {
		before, err := roomChain(ctx, tx, roomID)
		if err != nil {
			return err
		}
		ch = before
		var said []string
		if c.Budget != nil && *c.Budget != ch.Budget {
			said = append(said, fmt.Sprintf("set the chain budget to %d", *c.Budget))
			ch.Budget = *c.Budget
		}
		if c.Rearm && ch.Count > 0 {
			ch, _ = ch.On(ResetRearm)
			said = append(said, "re-armed the agent chain")
		}
		if c.Suspended != nil && *c.Suspended != ch.Suspended {
			if *c.Suspended {
				ch, _ = ch.On(Suspend)
				said = append(said, "suspended the chain limit: agents may call each other without end")
			} else {
				ch, _ = ch.On(Unsuspend)
				said = append(said, fmt.Sprintf("restored the chain limit of %d", ch.Budget))
			}
		}
		if len(said) == 0 {
			return nil
		}
		if err := saveChain(ctx, tx, roomID, ch); err != nil {
			return err
		}
		m, err = memberLine(ctx, tx, roomID, by, strings.Join(said, ", ")+".")
		return err
	})
	return ch, m, err
}

// EnqueueLink queues q, an agent-triggered turn, as one link of the room's chain. At the budget
// the link is dropped, never held: ErrChainPaused. ErrAlreadyQueued when its persona waits
// already. Neither counts.
func (s *Store) EnqueueLink(ctx context.Context, q *QueuedTurn, e ChainEvent) (Chain, error) {
	var c Chain
	err := s.withTx(ctx, "enqueue link", func(tx *sql.Tx) error {
		q.Source = string(e)
		if err := enqueue(ctx, tx, q); err != nil {
			return err
		}
		var ok bool
		var err error
		if c, ok, err = chainOn(ctx, tx, q.RoomID, e); err != nil {
			return err
		}
		if !ok {
			return ErrChainPaused
		}
		return nil
	})
	return c, err
}
