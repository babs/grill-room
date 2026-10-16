package web

import (
	"context"
	"errors"
	"net/http"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/store"
)

// Why the room's AI host app cannot take a website approval now (014-AC9).
const (
	msgHostAppOffline = "AI host app offline"
	msgHostAppOld     = "Update the AI host app"
)

// hostEgress says why the room's AI host app cannot take a website approval now, "" when it can.
func (s *Server) hostEgress(room string, host int64) string {
	switch h := s.hostDaemon(room, host); {
	case h == nil:
		return msgHostAppOffline
	case !h.Egress:
		return msgHostAppOld
	}
	return ""
}

// recordBlocked posts the websites a turn of persona was refused, one line each (014-AC8). The
// daemon sends domains only; one that is no allowlist entry, an IP or an Anthropic host, is dropped.
func (s *Server) recordBlocked(ctx context.Context, room, persona string, domains []string) {
	var keep []string
	seen := map[string]bool{}
	for _, d := range domains {
		list, err := sandbox.NormalizeAllowlist([]string{d})
		if err != nil || len(list) != 1 || list[0][0] == '.' || seen[list[0]] || len(keep) == maxBlockedPerTurn {
			continue
		}
		seen[list[0]] = true
		keep = append(keep, list[0])
	}
	if len(keep) == 0 {
		return
	}
	if _, err := s.bus.Store.RecordBlocked(ctx, room, persona, keep); err != nil {
		s.log.Error("record_blocked_failed", zap.String("room", room), zap.Error(err))
		return
	}
	s.bus.Notify(room)
}

// maxBlockedPerTurn bounds the lines one turn posts: the daemon reports what the sandbox tried.
const maxBlockedPerTurn = 20

// allowBlocked approves the website of a blocked line, for the room or for every room of the AI
// host's server (014-AC10, 014-AC11). The AI host may do either; a room admin only for the room,
// and only while the host lets them (014-AC12, 014-AC13). The approval waits for the host's app,
// which applies it and confirms: only then does the line say so.
func (s *Server) allowBlocked(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		Scope string `json:"scope"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Scope != store.EgressRoom && req.Scope != store.EgressServer {
		httpError(w, http.StatusBadRequest, "scope must be room or server")
		return
	}
	id, ok := messageID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	room, err := s.bus.Store.Room(ctx, sess.p.RoomID)
	if err != nil {
		s.serverError(w, "allow_blocked_failed", err)
		return
	}
	m, err := s.bus.Store.BlockedLine(ctx, room.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such blocked website")
		return
	}
	if err != nil {
		s.serverError(w, "allow_blocked_failed", err)
		return
	}
	host, err := s.bus.Store.HostsAs(ctx, room.ID, sess.p)
	if err != nil {
		s.serverError(w, "host_check_failed", err)
		return
	}
	if !host {
		admin, err := s.roomAdmin(ctx, &sess)
		switch {
		case err != nil:
			s.serverError(w, "room_admin_lookup_failed", err)
			return
		case !admin:
			httpError(w, http.StatusForbidden, "only the AI host, or a room admin they let, can allow a website")
			return
		case !room.EgressAdmins:
			httpError(w, http.StatusForbidden, "the AI host has not let room admins allow websites")
			return
		case req.Scope != store.EgressRoom:
			httpError(w, http.StatusForbidden, "only the AI host can allow a website for every room")
			return
		}
	}
	if _, err := sandbox.NormalizeAllowlist([]string{m.Blocked.Domain}); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if why := s.hostEgress(room.ID, room.HostParticipantID); why != "" {
		httpError(w, http.StatusConflict, why)
		return
	}
	m, err = s.bus.Store.RequestAllow(ctx, room.ID, id, req.Scope, sess.p)
	if errors.Is(err, store.ErrDecided) {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.serverError(w, "allow_blocked_failed", err)
		return
	}
	s.log.Info("egress_allow_requested", zap.String("room", room.ID), zap.Int64("message", id), zap.String("scope", req.Scope))
	s.bus.Notify(room.ID)
	writeJSON(w, http.StatusAccepted, viewMessage(*m))
}

// setEgressDelegation lets the room's admins allow websites for the room, or stops it; the AI host
// only (014-AC12).
func (s *Server) setEgressDelegation(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		On *bool `json:"on"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.On == nil {
		httpError(w, http.StatusBadRequest, "on is required")
		return
	}
	room, ok := s.asHost(w, r, sess)
	if !ok {
		return
	}
	m, err := s.bus.Store.SetEgressDelegation(r.Context(), room.ID, *req.On, sess.p)
	if err != nil {
		s.serverError(w, "egress_delegation_failed", err)
		return
	}
	if m != nil {
		s.bus.Notify(room.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// confirmAllow is the host daemon's answer to an approval it applied, or could not (014-AC10).
func (s *Server) confirmAllow(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	if !s.asHostDaemon(w, r, d) {
		return
	}
	var req struct {
		Error string `json:"error"`
	}
	if !decode(w, r, &req) {
		return
	}
	id, ok := messageID(w, r)
	if !ok {
		return
	}
	failure, _ := cleanText(truncate(req.Error, maxReason), maxReason, false)
	if req.Error != "" && failure == "" {
		failure = "the AI host's app refused it"
	}
	if _, err := s.bus.Store.ConfirmAllow(r.Context(), d.RoomID, id, failure); errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no approval waits on this line")
		return
	} else if err != nil {
		s.serverError(w, "confirm_allow_failed", err)
		return
	}
	s.bus.Notify(d.RoomID)
	w.WriteHeader(http.StatusNoContent)
}

// egressRevoked is the host daemon telling that its host removed a website from this room's list,
// or from every room's (014-AC15).
func (s *Server) egressRevoked(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	if !s.asHostDaemon(w, r, d) {
		return
	}
	var req struct {
		Domain string `json:"domain"`
		Scope  string `json:"scope"`
	}
	if !decode(w, r, &req) {
		return
	}
	list, err := sandbox.NormalizeAllowlist([]string{req.Domain})
	if err != nil || len(list) != 1 || req.Scope != store.EgressRoom && req.Scope != store.EgressServer {
		httpError(w, http.StatusBadRequest, "a domain and a scope, room or server")
		return
	}
	if _, err := s.bus.Store.EgressRevoked(r.Context(), d.RoomID, list[0], req.Scope); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.serverError(w, "egress_revoked_failed", err)
		return
	}
	s.bus.Notify(d.RoomID)
	w.WriteHeader(http.StatusNoContent)
}

// roomConnector is a claude.ai connector on for the room, as its host's daemon reports it.
type roomConnector struct {
	Name    string `json:"name"`
	Actions bool   `json:"actions,omitempty"`
}

// maxConnectors bounds what a daemon reports: the account's connectors number in the tens.
const maxConnectors = 50

// cleanConnectors keeps what the room shows of a daemon's report: names as text, bounded.
func cleanConnectors(in []roomConnector) []roomConnector {
	var out []roomConnector
	for _, c := range in[:min(len(in), maxConnectors)] {
		if name, _ := cleanText(truncate(c.Name, maxNameLen), maxNameLen, false); name != "" {
			out = append(out, roomConnector{Name: name, Actions: c.Actions})
		}
	}
	return out
}

// connectorEvent is the host daemon telling what its host changed of the room's connectors: a chat
// line and an audit entry as the host (014-AC19, 014-AC23).
func (s *Server) connectorEvent(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	if !s.asHostDaemon(w, r, d) {
		return
	}
	var req struct {
		Name    string `json:"name"`
		Kind    string `json:"kind"`
		Actions bool   `json:"actions"`
	}
	if !decode(w, r, &req) {
		return
	}
	name, _ := cleanText(truncate(req.Name, maxNameLen), maxNameLen, false)
	if name == "" {
		httpError(w, http.StatusBadRequest, "a connector name")
		return
	}
	if _, err := s.bus.Store.ConnectorChanged(r.Context(), d.RoomID, name, req.Kind, req.Actions); errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusBadRequest, "kind must be enable, disable or actions")
		return
	} else if err != nil {
		s.serverError(w, "connector_event_failed", err)
		return
	}
	s.bus.Notify(d.RoomID)
	w.WriteHeader(http.StatusNoContent)
}
