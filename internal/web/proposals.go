package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
)

// ActionProposalDismiss audits a related room proposal put aside (023-AC19); creating one audits
// as ActionRoomCreate.
const ActionProposalDismiss = "room_proposal.dismiss"

// pushAdded is a push telling someone a member seated them in a new room (023-AC18).
const pushAdded = "added"

const (
	msgHandled         = "Already handled"
	msgProposalProfile = "Only a member with a profile creates a room"
	msgProposalDismiss = "Only who started the turn or a room admin dismisses it"
	msgNoSuchProposal  = "no such proposal"
)

func proposalID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("proposal"), 10, 64)
	return id
}

// createFromProposal creates the room a persona proposed, for a member with a profile, under their
// right to create rooms (023-AC18): seats for the members they ticked, and the links when they
// administer this room.
func (s *Server) createFromProposal(w http.ResponseWriter, r *http.Request, sess session) {
	if sess.p.ProfileID == 0 {
		httpError(w, http.StatusForbidden, msgProposalProfile)
		return
	}
	p, err := s.bus.Store.ProfileByID(r.Context(), sess.p.ProfileID)
	if err != nil {
		s.serverError(w, "create_room_failed", err)
		return
	}
	if s.refuseExternal(w, p) {
		return
	}
	gen, refused := s.refuseAtQuota(w)
	if refused {
		return
	}
	var req struct {
		Name    string  `json:"name"`
		Members []int64 `json:"members"`
		Reads   bool    `json:"reads"`
		ReadBy  bool    `json:"read_by"`
	}
	if !decode(w, r, &req) {
		return
	}
	name, ok := cleanText(req.Name, maxRoomName, false)
	if !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("name must be 1 to %d characters", maxRoomName))
		return
	}
	if len(req.Members) > store.MaxProposalMembers {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("at most %d members", store.MaxProposalMembers))
		return
	}
	room := sess.p.RoomID
	id := strings.ToLower(NewToken())
	e := store.AuditEntry{At: s.now(), Actor: ActorProfile + p.UUID, Action: ActionRoomCreate, TargetKind: store.TargetRoom, TargetID: id, TargetName: name}
	ctx := store.WithAudit(r.Context(), e, map[string]any{"as": sess.p.Name, "proposal": proposalID(r), "proposed_in": room})
	link := NewToken()
	in := store.ProposedRoom{ID: id, Name: name, By: sess.p, InviteHash: HashToken(NewToken()), TokenHash: HashToken(NewToken()), Link: link, LinkHash: HashToken(link),
		Members: req.Members, SeatHash: func() string { return HashToken(NewToken()) }, Reads: req.Reads, ReadBy: req.ReadBy}
	created, seats, links, err := s.bus.Store.CreateProposedRoom(ctx, room, proposalID(r), in)
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, msgNoSuchProposal)
		return
	case errors.Is(err, store.ErrHandled):
		httpError(w, http.StatusConflict, msgHandled)
		return
	case errors.Is(err, store.ErrCreationRefused):
		s.creationRefused(w, err, gen)
		return
	case errors.Is(err, store.ErrLinkCap):
		httpError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.serverError(w, "create_room_failed", err)
		return
	}
	s.bus.Notify(room)
	s.bus.Notify(created.ID)
	s.pushAdded(podContext(r), room, created.ID, seats)
	s.log.Info("room_created", zap.String("room", created.ID), zap.Int64("profile", p.ID), zap.String("proposed_in", room),
		zap.Int("seats", len(seats)), zap.Int("links", len(links)))
	writeJSON(w, http.StatusCreated, created)
}

// pushAdded tells each seat's browsers, subscribed in room, that they sit in the new room now
// (023-AC18), from one goroutine that outlives the request, not its pod (ctx). Their new seats have no subscription
// yet; the payload names the new room only, as every push. A member looking at room sees the card
// change instead, as no push goes to a focused tab.
func (s *Server) pushAdded(ctx context.Context, room, newRoom string, seats []store.Seated) {
	if len(seats) == 0 {
		return
	}
	go func() {
		subs, err := s.bus.Store.PushSubscriptions(ctx, room)
		if err != nil {
			s.log.Warn("push_read_failed", zap.Error(err))
			return
		}
		v, name, ok := s.pushFor(ctx, newRoom)
		if !ok {
			return
		}
		of := map[int64][]store.PushSubscription{}
		for _, sub := range subs {
			of[sub.ParticipantID] = append(of[sub.ParticipantID], sub)
		}
		for _, seat := range seats {
			if s.people.focused(room, seat.From, s.lease()) {
				continue
			}
			for _, sub := range of[seat.From] {
				s.deliver(ctx, v, name, &pushBatch{sub: sub, room: newRoom, kind: pushAdded, count: 1, message: seat.Line})
			}
		}
	}()
}

// dismissProposal puts a proposal aside, for who started its turn or a room admin (023-AC19).
func (s *Server) dismissProposal(w http.ResponseWriter, r *http.Request, sess session) {
	admin, err := s.roomAdmin(r.Context(), &sess)
	if err != nil {
		s.serverError(w, "room_admin_lookup_failed", err)
		return
	}
	room := sess.p.RoomID
	// The store names the actor: who pressed may hold no profile.
	e := store.AuditEntry{At: s.now(), Action: ActionProposalDismiss, TargetKind: store.TargetRoom, TargetID: room}
	ctx := store.WithAudit(r.Context(), e, map[string]any{"as": sess.p.Name, "participant": sess.p.ID})
	err = s.bus.Store.DismissProposal(ctx, room, proposalID(r), sess.p, admin)
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, msgNoSuchProposal)
	case errors.Is(err, store.ErrForbidden):
		httpError(w, http.StatusForbidden, msgProposalDismiss)
	case errors.Is(err, store.ErrHandled):
		httpError(w, http.StatusConflict, msgHandled)
	case err != nil:
		s.serverError(w, "dismiss_proposal_failed", err)
	default:
		s.bus.Notify(room)
		w.WriteHeader(http.StatusNoContent)
	}
}

// markProposedIn tells whether the caller of sess sits in the room pi names, so its name links
// there (023-AC20).
func (s *Server) markProposedIn(ctx context.Context, pi *store.ProposedIn, sess session) error {
	if pi == nil || pi.Room == "" {
		return nil
	}
	seated, err := s.bus.Store.SeatedRooms(ctx, sess.p.RoomID, sess.p.ID)
	if err != nil {
		return err
	}
	pi.Member = seated[pi.Room]
	return nil
}
