package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
)

// APITokenPrefix starts every personal API token (012-AC1): the server tells them from the admin,
// participant and daemon tokens by it.
const APITokenPrefix = "grt_"

const (
	maxTokenName = 60
	// tokenTouchEvery throttles the last-use write of a token (012-AC9).
	tokenTouchEvery = time.Minute
)

// Audit actions of personal API tokens (012-AC10).
const (
	ActionAPITokenCreate = "api_token.create"
	ActionAPITokenRevoke = "api_token.revoke"
)

// tokenExpiries are the lifetimes a token may have, in days; 0 never expires (012-AC1).
var tokenExpiries = []int{30, 90, 365, 0}

// cookieOnly are the routes a token never reaches, as they make another lasting credential or
// sign a browser in (012-AC6).
var cookieOnly = map[string]bool{
	"GET /api/profile/tokens":              true,
	"POST /api/profile/tokens":             true,
	"DELETE /api/profile/tokens/{id}":      true,
	"POST /api/join":                       true,
	"POST /api/profile/email":              true,
	"POST /api/profile/signin":             true,
	"POST /api/profile/magic":              true,
	"POST /api/profile/recover":            true,
	"POST /api/rooms/{room}/pairings":      true,
	"GET /api/pairings/{id}":               true,
	"GET /api/rooms/{room}/pairings/{id}":  true,
	"POST /api/rooms/{room}/pairings/{id}": true,
	"GET /api/rooms/{room}/push":           true,
	"PUT /api/rooms/{room}/push":           true,
	"DELETE /api/rooms/{room}/push":        true,
}

const (
	msgTokenAdmin    = "API tokens never reach admin routes"
	msgTokenCookie   = "API tokens cannot do this; use the browser" //nolint:gosec // G101: an error message
	msgTokenReadOnly = "This token is read only"                    //nolint:gosec // G101: an error message
	msgTokenExpired  = "This token has expired"
	msgTokenNotHere  = "Your profile is not in this room"
)

type apiTokenKey struct{}

// apiTokenOf returns the API token r came with, nil for none.
func apiTokenOf(r *http.Request) *store.APIToken {
	t, _ := r.Context().Value(apiTokenKey{}).(*store.APIToken)
	return t
}

// tokenGate checks a request carrying an API token before any route runs: the token must be
// live (012-AC7, 012-AC8), a read-only one may only read (012-AC4), and admin and cookie-only
// routes refuse it (012-AC5, 012-AC6). It records the use (012-AC9) and passes the token on.
func (s *Server) tokenGate(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if !strings.HasPrefix(tok, APITokenPrefix) {
			mux.ServeHTTP(w, r)
			return
		}
		now := s.now()
		t, err := s.bus.Store.APITokenByHash(r.Context(), HashToken(tok), now)
		switch {
		case errors.Is(err, store.ErrNotFound):
			httpError(w, http.StatusUnauthorized, "invalid or revoked token")
			return
		case errors.Is(err, store.ErrTokenExpired):
			httpError(w, http.StatusUnauthorized, msgTokenExpired)
			return
		case err != nil:
			s.serverError(w, "api_token_lookup_failed", err)
			return
		}
		h, pattern := mux.Handler(r)
		_, adminOnly := h.(adminHandler)
		switch {
		case adminOnly || strings.HasPrefix(r.URL.Path, "/api/admin/"):
			httpError(w, http.StatusForbidden, msgTokenAdmin)
			return
		case cookieOnly[pattern]:
			httpError(w, http.StatusForbidden, msgTokenCookie)
			return
		// /mcp is POST for every call; its sub-requests come back through here with the token.
		case t.Scope == store.ScopeRead && r.Method != http.MethodGet && r.Method != http.MethodHead && pattern != "POST /mcp":
			httpError(w, http.StatusForbidden, msgTokenReadOnly)
			return
		}
		if t.LastUsedAt == nil || now.Sub(*t.LastUsedAt) >= tokenTouchEvery {
			if _, err := s.bus.Store.TouchAPIToken(r.Context(), t.ID, s.clientIP(r), now, tokenTouchEvery); err != nil && r.Context().Err() == nil {
				s.log.Warn("api_token_touch_failed", zap.Int64("api_token", t.ID), zap.Error(err))
			}
		}
		ctx := store.WithAPIToken(context.WithValue(r.Context(), apiTokenKey{}, t), t.ID)
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sessionProfile returns the profile of the request's API token or, without one, of its profile
// cookie; nil when it has neither, or a stale cookie.
func (s *Server) sessionProfile(r *http.Request) (*store.Profile, error) {
	t := apiTokenOf(r)
	if t == nil {
		return s.profileFromCookie(r)
	}
	p, err := s.bus.Store.ProfileByID(r.Context(), t.ProfileID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return p, err
}

// tokenAudit returns r's context carrying the entry of a token change of profile p.
func (s *Server) tokenAudit(r *http.Request, p *store.Profile, action string) context.Context {
	e := store.AuditEntry{At: s.now(), Actor: ActorProfile + p.UUID, Action: action, TargetKind: store.TargetProfile, TargetID: strconv.FormatInt(p.ID, 10)}
	return store.WithAudit(r.Context(), e, nil)
}

// listTokens lists the profile's tokens for its own browsers (012-AC2).
func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	p := s.profileFor(w, r)
	if p == nil {
		return
	}
	ts, err := s.bus.Store.APITokens(r.Context(), p.ID)
	if err != nil {
		s.serverError(w, "list_api_tokens_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": ts, "url": s.baseURL})
}

type createdToken struct {
	*store.APIToken
	// Token is shown once: only its hash is kept (012-AC1).
	Token string `json:"token"`
}

// createToken makes a personal API token (012-AC1, 012-AC10, 012-AC11).
func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	p := s.profileFor(w, r)
	if p == nil {
		return
	}
	var req struct {
		Name    string `json:"name"`
		Scope   string `json:"scope"`
		Expires *int   `json:"expires_days"`
	}
	if !decode(w, r, &req) {
		return
	}
	name, ok := cleanText(req.Name, maxTokenName, false)
	if !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("name must be 1 to %d characters on one line", maxTokenName))
		return
	}
	if req.Scope != store.ScopeRead && req.Scope != store.ScopeFull {
		httpError(w, http.StatusBadRequest, "scope must be read or full")
		return
	}
	days := 90
	if req.Expires != nil {
		days = *req.Expires
	}
	if !slices.Contains(tokenExpiries, days) {
		httpError(w, http.StatusBadRequest, "expires_days must be 30, 90, 365, or 0 for never")
		return
	}
	now := s.now()
	var expires *time.Time
	if days > 0 {
		e := now.Add(time.Duration(days) * 24 * time.Hour)
		expires = &e
	}
	tok := APITokenPrefix + NewToken()
	t, err := s.bus.Store.CreateAPIToken(s.tokenAudit(r, p, ActionAPITokenCreate), p.ID, name, HashToken(tok), req.Scope, expires, now)
	switch {
	case errors.Is(err, store.ErrLimit):
		httpError(w, http.StatusConflict, fmt.Sprintf("You have %d API tokens. Revoke one first.", store.MaxAPITokens))
		return
	case err != nil:
		s.serverError(w, "create_api_token_failed", err)
		return
	}
	s.log.Info("api_token_created", zap.Int64("api_token", t.ID), zap.Int64("profile", p.ID), zap.String("scope", t.Scope))
	writeJSON(w, http.StatusCreated, createdToken{APIToken: t, Token: tok})
}

// revokeToken deletes one of the profile's tokens at once (012-AC8, 012-AC10).
func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	p := s.profileFor(w, r)
	if p == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		err = s.bus.Store.RevokeAPIToken(s.tokenAudit(r, p, ActionAPITokenRevoke), p.ID, id)
	}
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound), errors.Is(err, strconv.ErrSyntax), errors.Is(err, strconv.ErrRange):
		httpError(w, http.StatusNotFound, "no such token")
		return
	default:
		s.serverError(w, "revoke_api_token_failed", err)
		return
	}
	s.log.Info("api_token_revoked", zap.Int64("api_token", id), zap.Int64("profile", p.ID))
	w.WriteHeader(http.StatusNoContent)
}
