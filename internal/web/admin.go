package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/uivendor"
)

const (
	adminCookie = "grill_room_admin"
	// adminCookiePath covers every route the /admin page calls, and nothing else.
	adminCookiePath = "/api/admin"
	// Failed logins allowed per client IP within loginWindow (004-AC1).
	loginAttempts = 10
	loginWindow   = 15 * time.Minute
	maxUserAgent  = 256
	auditPage     = 50
	maxAuditPage  = 200
	// DefaultAuditRetention is how long the audit log keeps an entry (004-AC6).
	DefaultAuditRetention = 365 * 24 * time.Hour
)

// auditPurgeEvery is how often the audit log is purged. A var for tests.
var auditPurgeEvery = 24 * time.Hour

// Audit actions (004-AC6).
const (
	ActionRoomCreate     = "room.create"
	ActionRoomRename     = "room.rename"
	ActionRoomDelete     = "room.delete"
	ActionInviteCreate   = "invite.create"
	ActionInviteRevoke   = "invite.revoke"
	ActionProfileDelete  = "profile.delete"
	ActionTemplateRemove = "template.remove"
	ActionSkillRemove    = "skill.remove"
	ActionAdminLogin     = "admin.login"
	ActionAdminLogout    = "admin.logout"
	ActionSessionRevoke  = "admin.session_revoke"
)

// AuditActions lists every action, for the audit view's filter.
var AuditActions = []string{ActionRoomCreate, ActionRoomRename, ActionRoomDelete, ActionInviteCreate, ActionInviteRevoke,
	ActionProfileDelete, ActionTemplateRemove, ActionSkillRemove, ActionAdminLogin, ActionAdminLogout, ActionSessionRevoke}

// adminCSP admits the /admin page's import map by its hash.
var adminCSP = func() string {
	page, _ := ui.ReadFile("ui/admin.html") // embedded
	return uivendor.CSP(page, "'none'")
}()

type adminKey struct{}

// adminCaller is how a request proved the admin: the token (Session 0) or an /admin session.
type adminCaller struct{ Session int64 }

func callerOf(r *http.Request) adminCaller {
	c, _ := r.Context().Value(adminKey{}).(adminCaller)
	return c
}

// asAdmin returns r carrying how it proved the admin, nil when it does not. API clients send the
// token as a bearer; the /admin page holds a session cookie. answered is set when asAdmin already
// wrote the response.
func (s *Server) asAdmin(w http.ResponseWriter, r *http.Request) (admin *http.Request, answered bool) {
	if tok := bearer(r); tok != "" {
		if subtle.ConstantTimeCompare([]byte(HashToken(tok)), s.adminHash) != 1 {
			return nil, false
		}
		return r.WithContext(context.WithValue(r.Context(), adminKey{}, adminCaller{})), false
	}
	c, err := r.Cookie(adminCookie)
	if err != nil || c.Value == "" {
		return nil, false
	}
	if crossSite(r) {
		httpError(w, http.StatusForbidden, "cross-site request refused")
		return nil, true
	}
	id, err := s.bus.Store.TouchAdminSession(r.Context(), HashToken(c.Value), s.now())
	if errors.Is(err, store.ErrNotFound) {
		return nil, false
	}
	if err != nil {
		s.serverError(w, "admin_session_lookup_failed", err)
		return nil, true
	}
	return r.WithContext(context.WithValue(r.Context(), adminKey{}, adminCaller{Session: id})), false
}

func (s *Server) admin(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ar, answered := s.asAdmin(w, r)
		if answered {
			return
		}
		if ar == nil {
			httpError(w, http.StatusUnauthorized, "admin token or session required")
			return
		}
		next(w, ar)
	})
}

// memberOrAdmin runs next for the admin with a nil session, else for a room participant.
func (s *Server) memberOrAdmin(next func(http.ResponseWriter, *http.Request, *session)) http.Handler {
	member := s.participant(func(w http.ResponseWriter, r *http.Request, sess session) { next(w, r, &sess) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ar, answered := s.asAdmin(w, r)
		switch {
		case answered:
		case ar != nil:
			next(w, ar, nil)
		default:
			member.ServeHTTP(w, r)
		}
	})
}

// audit records an admin action that took place, under the request's admin caller. The write
// outlives a client gone meanwhile; a failed one is logged with the entry.
func (s *Server) audit(r *http.Request, action, kind, id, name string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	if c := callerOf(r); c.Session != 0 {
		details["via"], details["session"] = "session", c.Session
	} else {
		details["via"] = "token"
	}
	e := store.AuditEntry{At: s.now(), Actor: store.ActorSuperAdmin, Action: action, TargetKind: kind, TargetID: id, TargetName: name}
	var err error
	if e.Details, err = json.Marshal(details); err == nil {
		err = s.bus.Store.Audit(context.WithoutCancel(r.Context()), e)
	}
	if err != nil {
		s.log.Error("audit_write_failed", zap.String("action", action), zap.String("target_kind", kind), zap.String("target_id", id), zap.Error(err))
	}
}

// targetName is the name an audit entry keeps for a target about to change or go.
func (s *Server) targetName(r *http.Request, kind, id string) string {
	name, err := s.bus.Store.AuditTargetName(r.Context(), kind, id)
	if err != nil {
		s.log.Error("audit_target_name_failed", zap.String("target_kind", kind), zap.String("target_id", id), zap.Error(err))
	}
	return name
}

func (s *Server) adminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", adminCSP)
	http.ServeFileFS(w, r, ui, "ui/admin.html")
}

func setAdminCookie(w http.ResponseWriter, r *http.Request, tok string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookie,
		Value:    tok,
		Path:     adminCookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure(r),
		SameSite: http.SameSiteStrictMode,
	})
}

// clip cuts s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// adminLogin trades the admin token for a session cookie. Each client IP gets loginAttempts tries
// per loginWindow; a success clears its count.
func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	if crossSite(r) {
		httpError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &req) {
		return
	}
	now, ip := s.now(), clientIP(r)
	key := "ip:" + ip
	// Counted before the check, in one step: concurrent guesses cannot slip past the limit.
	if !s.loginLimit.allow(key, loginAttempts, now) {
		httpError(w, http.StatusTooManyRequests, "too many failed logins, retry in 15 minutes")
		return
	}
	if req.Token == "" || subtle.ConstantTimeCompare([]byte(HashToken(req.Token)), s.adminHash) != 1 {
		s.log.Warn("admin_login_failed", zap.String("ip", ip))
		httpError(w, http.StatusUnauthorized, "wrong admin token")
		return
	}
	s.loginLimit.reset(key)
	tok := NewToken()
	sess, err := s.bus.Store.CreateAdminSession(r.Context(), HashToken(tok), clip(r.UserAgent(), maxUserAgent), ip, now)
	if err != nil {
		s.serverError(w, "admin_login_failed", err)
		return
	}
	setAdminCookie(w, r, tok, int(store.AdminSessionTTL/time.Second))
	ar := r.WithContext(context.WithValue(r.Context(), adminKey{}, adminCaller{Session: sess.ID}))
	s.audit(ar, ActionAdminLogin, store.TargetAdminSession, strconv.FormatInt(sess.ID, 10), "", map[string]any{"ip": ip})
	s.log.Info("admin_signed_in", zap.Int64("session", sess.ID))
	writeJSON(w, http.StatusCreated, sess)
}

// adminLogout ends the caller's own session and clears its cookie.
func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request) {
	if id := callerOf(r).Session; id != 0 {
		if err := s.bus.Store.RevokeAdminSession(r.Context(), id); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.serverError(w, "admin_logout_failed", err)
			return
		}
		s.audit(r, ActionAdminLogout, store.TargetAdminSession, strconv.FormatInt(id, 10), "", nil)
	}
	setAdminCookie(w, r, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

type adminSessionView struct {
	store.AdminSession
	// Current marks the session the request came with.
	Current bool `json:"current"`
}

func (s *Server) adminSessions(w http.ResponseWriter, r *http.Request) {
	list, err := s.bus.Store.AdminSessions(r.Context(), s.now())
	if err != nil {
		s.serverError(w, "list_admin_sessions_failed", err)
		return
	}
	cur := callerOf(r).Session
	out := make([]adminSessionView, len(list))
	for i, a := range list {
		out[i] = adminSessionView{AdminSession: a, Current: a.ID == cur}
	}
	writeJSON(w, http.StatusOK, out)
}

// revokeAdminSession ends any session; its next request gets a 401.
func (s *Server) revokeAdminSession(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		err = s.bus.Store.RevokeAdminSession(r.Context(), id)
	}
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, strconv.ErrSyntax) || errors.Is(err, strconv.ErrRange) {
		httpError(w, http.StatusNotFound, "no such session")
		return
	}
	if err != nil {
		s.serverError(w, "revoke_admin_session_failed", err)
		return
	}
	s.audit(r, ActionSessionRevoke, store.TargetAdminSession, strconv.FormatInt(id, 10), "", nil)
	if id == callerOf(r).Session {
		setAdminCookie(w, r, "", -1)
	}
	s.log.Info("admin_session_revoked", zap.Int64("session", id))
	w.WriteHeader(http.StatusNoContent)
}

type auditResp struct {
	Entries []store.AuditEntry `json:"entries"`
	// Next is the ?before= of the next page, 0 on the last one.
	Next    int64    `json:"next"`
	Actions []string `json:"actions"`
}

// auditLog pages through the audit log, newest first: ?action=, ?target= (an id, or part of a
// name), ?before= and ?limit=.
func (s *Server) auditLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	aq := store.AuditQuery{Action: q.Get("action"), Target: q.Get("target"), Limit: auditPage}
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			httpError(w, http.StatusBadRequest, "before must be an entry id")
			return
		}
		aq.Before = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxAuditPage {
			httpError(w, http.StatusBadRequest, "limit must be 1 to "+strconv.Itoa(maxAuditPage))
			return
		}
		aq.Limit = n
	}
	entries, err := s.bus.Store.AuditLog(r.Context(), aq)
	if err != nil {
		s.serverError(w, "audit_log_failed", err)
		return
	}
	resp := auditResp{Entries: entries, Actions: AuditActions}
	if len(entries) == aq.Limit {
		resp.Next = entries[len(entries)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

// PurgeAudit deletes the audit entries past the retention and the expired admin sessions now, then
// every auditPurgeEvery, until ctx ends (004-AC6).
func (s *Server) PurgeAudit(ctx context.Context) {
	for {
		now := s.now()
		n, err := s.bus.Store.PurgeAudit(ctx, now.Add(-s.opts.AuditRetention))
		switch {
		case err != nil && ctx.Err() == nil:
			s.log.Error("audit_purge_failed", zap.Error(err))
		case n > 0:
			s.log.Info("audit_purged", zap.Int64("entries", n))
		}
		if _, err := s.bus.Store.PurgeAdminSessions(ctx, now); err != nil && ctx.Err() == nil {
			s.log.Error("admin_session_purge_failed", zap.Error(err))
		}
		t := time.NewTimer(auditPurgeEvery)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}
