package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/webpush"
)

// Web Push (005-AC24): a participant's browsers get a notification for a room when none of their
// tabs there is focused. Kinds, from the most to the least telling: a mention of them, a turn they
// started that ended without a synthesis, a synthesis, and, opted in, any other line.
const (
	pushMention   = "mention"
	pushTurn      = "turn"
	pushSynthesis = "synthesis"
	pushMessage   = "message"
	// vapidSetting holds the VAPID private key, generated on first use.
	vapidSetting = "vapid_private_key"
	// pushCursorSetting holds the last line id the push loop handled (017-AC17).
	pushCursorSetting = "push_cursor"
	// pushSends bounds the pushes in flight at once.
	pushSends = 4
)

var pushRank = map[string]int{pushMention: 3, pushTurn: 2, pushSynthesis: 1, pushMessage: 0}

// DefaultPushHosts are the push services a subscription may name, by host suffix: Chrome and
// Android, Firefox, Safari and Edge. A subscription is a URL the server posts to, so any other
// host is refused, an internal one above all.
var DefaultPushHosts = []string{"fcm.googleapis.com", "push.services.mozilla.com", "push.apple.com", "notify.windows.com"}

// pushWindow is how long events of one room gather into one push per browser, and pushTick how
// often the gatherer looks. Vars for tests.
var (
	pushWindow = 5 * time.Second
	pushTick   = time.Second
)

// pushHostAllowed reports whether endpoint's host is one of hosts or below one.
func pushHostAllowed(endpoint string, hosts []string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return slices.ContainsFunc(hosts, func(s string) bool { return h == s || strings.HasSuffix(h, "."+s) })
}

// vapidKey loads the instance's VAPID key, generating and storing it on first use.
func (s *Server) vapidKey(ctx context.Context) (*webpush.VAPID, error) {
	s.vapidMu.Lock()
	defer s.vapidMu.Unlock()
	if s.vapid != nil {
		return s.vapid, nil
	}
	fresh, err := webpush.NewVAPIDKey()
	if err != nil {
		return nil, err
	}
	stored, err := s.bus.Store.SettingOrInit(ctx, vapidSetting, base64.StdEncoding.EncodeToString(fresh))
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return nil, fmt.Errorf("stored vapid key: %w", err)
	}
	// RFC 8292 asks for a contact: the instance's address, or a mailto that reaches nobody on a
	// plain-http dev instance, as push services refuse an http one.
	subject := s.baseURL
	if !strings.HasPrefix(subject, "https://") {
		subject = "mailto:grill-room@invalid"
	}
	if s.vapid, err = webpush.ParseVAPID(raw, subject); err != nil {
		return nil, err
	}
	return s.vapid, nil
}

// getPush answers the VAPID public key the browser subscribes with.
func (s *Server) getPush(w http.ResponseWriter, r *http.Request, _ session) {
	v, err := s.vapidKey(r.Context())
	if err != nil {
		s.serverError(w, "vapid_key_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": v.PublicKey()})
}

// putPush stores the browser's subscription for this participant (PUT), or removes it (DELETE).
func (s *Server) putPush(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		Subscription webpush.Subscription `json:"subscription"`
		AllMessages  bool                 `json:"all_messages"`
	}
	if !decode(w, r, &req) {
		return
	}
	sub := req.Subscription
	if r.Method == http.MethodDelete {
		if err := s.bus.Store.DeletePushSubscription(r.Context(), sess.p.ID, sub.Endpoint); err != nil {
			s.serverError(w, "push_unsubscribe_failed", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := sub.Check(); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !pushHostAllowed(sub.Endpoint, s.opts.PushHosts) {
		httpError(w, http.StatusBadRequest, "this push service is not one the instance sends to")
		return
	}
	err := s.bus.Store.SavePushSubscription(r.Context(), store.PushSubscription{ParticipantID: sess.p.ID, Endpoint: sub.Endpoint,
		P256dh: sub.Keys.P256dh, Auth: sub.Keys.Auth, AllMessages: req.AllMessages})
	if err != nil {
		s.serverError(w, "push_subscribe_failed", err)
		return
	}
	s.log.Info("push_subscribed", zap.String("room", sess.p.RoomID), zap.Int64("participant", sess.p.ID), zap.Bool("all_messages", req.AllMessages))
	w.WriteHeader(http.StatusNoContent)
}

// pushBatch is what gathered for one browser in one room since its first event.
type pushBatch struct {
	sub   store.PushSubscription
	room  string
	kind  string
	count int
	first time.Time
	// message is the latest line of the batch's kind: the notification opens the chat there.
	message int64
	// from is the batch's first line: the push cursor stays before it until the batch is sent.
	from int64
}

// pushPayload is all a notification says: it passes through the browser vendor's push service, so
// no line's text, no name but the room's. URL opens the chat at the line that triggered it.
type pushPayload struct {
	Room  string `json:"room"`
	Kind  string `json:"kind"`
	Count int    `json:"count"`
	URL   string `json:"url"`
}

// PushLoop sends the room's events as Web Push notifications until ctx ends (005-AC24). It reads
// every room's lines past the push cursor on each room signal, as an event stream does; the cursor
// in settings lets the next leader go on where this one stopped (017-AC17).
func (s *Server) PushLoop(ctx context.Context) {
	signals, cancel := s.bus.Subscribe(256)
	defer cancel()
	cursor, err := s.pushCursor(ctx)
	if err != nil {
		s.log.Error("push_start_failed", zap.Error(err))
		return
	}
	// The first tick writes the cursor: the next leader must find it even when it never moved.
	saved := int64(-1)
	pending := map[int64]*pushBatch{}
	// sending holds the batches in flight, whose lines the cursor must not pass yet.
	var mu sync.Mutex
	sending := map[*pushBatch]bool{}
	sends := make(chan struct{}, pushSends)
	var wg sync.WaitGroup
	defer wg.Wait()
	read := func() {
		last, err := s.pushLines(ctx, cursor, pending)
		if err != nil && ctx.Err() == nil {
			s.log.Warn("push_read_failed", zap.Error(err))
		}
		cursor = last
		if s.pushRead != nil {
			s.pushRead(last)
		}
	}
	// The lines said while no leader read them.
	read()
	tick := time.NewTicker(pushTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case room := <-signals:
			if pushable(room) {
				read()
			}
		case <-tick.C:
			// The server's clock, as each batch's first event used: the tick only polls.
			now := s.now()
			sent := 0
			for id, b := range pending {
				if now.Sub(b.first) < pushWindow {
					continue
				}
				delete(pending, id)
				// Back in the room meanwhile: they have seen it.
				if s.people.focused(b.room, b.sub.ParticipantID, s.lease()) {
					continue
				}
				select {
				case sends <- struct{}{}:
				case <-ctx.Done():
					return
				}
				mu.Lock()
				sending[b] = true
				mu.Unlock()
				wg.Add(1)
				sent++
				go func() {
					defer wg.Done()
					defer func() { <-sends }()
					s.sendPush(ctx, b)
					mu.Lock()
					delete(sending, b)
					mu.Unlock()
				}()
			}
			// ponytail: a leader that dies mid-send leaves its cursor before the batch, so the next
			// one sends it again: at least once.
			handled := cursor
			for _, b := range pending {
				handled = min(handled, b.from-1)
			}
			mu.Lock()
			for b := range sending {
				handled = min(handled, b.from-1)
			}
			mu.Unlock()
			if handled != saved {
				if err := s.bus.Store.SetSetting(ctx, pushCursorSetting, strconv.FormatInt(handled, 10)); err != nil {
					if ctx.Err() == nil {
						s.log.Warn("push_cursor_failed", zap.Error(err))
					}
				} else {
					saved = handled
				}
			}
			if s.pushTicked != nil {
				s.pushTicked(now, sent)
			}
		}
	}
}

// pushCursor is the last line id a push loop handled, from settings; the newest line when none
// did, as nothing said before the first start is pushed.
func (s *Server) pushCursor(ctx context.Context) (int64, error) {
	v, err := s.bus.Store.Setting(ctx, pushCursorSetting)
	if err != nil {
		return 0, err
	}
	if id, err := strconv.ParseInt(v, 10, 64); err == nil {
		return id, nil
	} else if v != "" {
		s.log.Warn("push_cursor_invalid", zap.String("value", v), zap.Error(err))
	}
	return s.bus.Store.LastMessageID(ctx)
}

// pushable tells the signals that may bring lines: a room's, and bus.All after a LISTEN
// reconnect, not the ':'-prefixed ids such as the TTS wakes.
func pushable(id string) bool { return !strings.HasPrefix(id, ":") }

// pushRoom is what classifying a room's lines reads once per room.
type pushRoom struct {
	subs     []store.PushSubscription
	ps       []store.Participant
	personas []store.RoomPersona
}

// pushLines classifies every room's lines past after for each subscription, into pending, and
// returns the last line id read. A read batch goes into pending whole or not at all, so a failed
// read leaves no line counted twice.
func (s *Server) pushLines(ctx context.Context, after int64, pending map[int64]*pushBatch) (int64, error) {
	st := s.bus.Store
	for {
		msgs, err := st.MessagesAfter(ctx, after, replayBatch)
		if err != nil || len(msgs) == 0 {
			return after, err
		}
		rooms := map[string]*pushRoom{}
		for _, m := range msgs {
			if rooms[m.RoomID] != nil {
				continue
			}
			r := &pushRoom{}
			if r.subs, err = st.PushSubscriptions(ctx, m.RoomID); err != nil {
				return after, err
			}
			if len(r.subs) > 0 {
				if r.ps, err = st.Participants(ctx, m.RoomID); err != nil {
					return after, err
				}
				if r.personas, err = st.RoomPersonas(ctx, m.RoomID); err != nil {
					return after, err
				}
			}
			rooms[m.RoomID] = r
		}
		now := s.now()
		for _, m := range msgs {
			if r := rooms[m.RoomID]; len(r.subs) > 0 {
				s.pushLine(ctx, r, m, now, pending)
			}
		}
		after = msgs[len(msgs)-1].ID
		if len(msgs) < replayBatch {
			return after, nil
		}
	}
}

// pushLine adds line m to the pending batch of each subscription of its room it concerns.
func (s *Server) pushLine(ctx context.Context, r *pushRoom, m store.Message, now time.Time, pending map[int64]*pushBatch) {
	starter := int64(0)
	if m.TurnID != 0 && m.Kind != store.KindText {
		if t, err := s.bus.Store.Turn(ctx, m.RoomID, m.TurnID); err == nil {
			starter = t.ParticipantID
		}
	}
	mentioned := mentionedPeople(m, r.personas, r.ps)
	for _, sub := range r.subs {
		kind := pushKind(m, sub, starter, mentioned[sub.ParticipantID])
		if kind == "" || s.people.focused(m.RoomID, sub.ParticipantID, s.lease()) {
			continue
		}
		b := pending[sub.ID]
		if b == nil {
			b = &pushBatch{sub: sub, room: m.RoomID, first: now, from: m.ID}
			pending[sub.ID] = b
		}
		b.count++
		if b.kind == "" || pushRank[kind] >= pushRank[b.kind] {
			b.kind, b.message = kind, m.ID
		}
	}
}

// pushKind is what line m is to sub's participant, "" for nothing to notify. starter started the
// turn m tells of; mentioned tells whether m names them.
func pushKind(m store.Message, sub store.PushSubscription, starter int64, mentioned bool) string {
	switch {
	case m.ParticipantID == sub.ParticipantID && m.Kind != store.KindSystem, m.DeletedAt != nil:
		return ""
	case m.Kind == store.KindSystem:
		// A turn that failed or was stopped: its starter learns it ended.
		if m.TurnID != 0 && starter == sub.ParticipantID {
			return pushTurn
		}
		return ""
	case mentioned:
		return pushMention
	case m.Kind == store.KindAgent:
		return pushSynthesis
	case sub.AllMessages:
		return pushMessage
	}
	return ""
}

// mentionedPeople returns the participants a text line or a synthesis names with their @token, by
// the rule the chips follow (005-AC27): a persona of the same name takes the mention.
func mentionedPeople(m store.Message, personas []store.RoomPersona, ps []store.Participant) map[int64]bool {
	out := map[int64]bool{}
	if m.Kind != store.KindText && m.Kind != store.KindAgent {
		return out
	}
	for _, n := range named(m.Body, personas, ps) {
		if n.ParticipantID != 0 {
			out[n.ParticipantID] = true
		}
	}
	return out
}

// sendPush delivers one batch, and drops a subscription the push service says is gone or refuses
// for good.
func (s *Server) sendPush(ctx context.Context, b *pushBatch) {
	v, err := s.vapidKey(ctx)
	if err != nil {
		s.log.Error("vapid_key_failed", zap.Error(err))
		return
	}
	r, err := s.bus.Store.Room(ctx, b.room)
	if err != nil {
		return
	}
	link := "/r/" + url.PathEscape(b.room) + "#m" + strconv.FormatInt(b.message, 10)
	payload, err := json.Marshal(pushPayload{Room: r.Name, Kind: b.kind, Count: b.count, URL: link})
	if err != nil {
		return
	}
	// The allowlist may have shrunk since the browser subscribed.
	if !pushHostAllowed(b.sub.Endpoint, s.opts.PushHosts) {
		return
	}
	sub := &webpush.Subscription{Endpoint: b.sub.Endpoint}
	sub.Keys.P256dh, sub.Keys.Auth = b.sub.P256dh, b.sub.Auth
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// The topic replaces a push still undelivered for the same room (RFC 8030 section 5.4).
	err = v.Send(ctx, s.pushClient, sub, webpush.Message{Payload: payload, TTL: 12 * time.Hour, Topic: pushTopic(b.room), Urgency: "normal"}, s.now())
	switch {
	case errors.Is(err, webpush.ErrGone):
		if err := s.bus.Store.DropPushSubscription(context.WithoutCancel(ctx), b.sub.ID); err != nil {
			s.log.Warn("push_drop_failed", zap.Int64("subscription", b.sub.ID), zap.Error(err))
		}
		s.log.Info("push_subscription_gone", zap.String("room", b.room), zap.Int64("participant", b.sub.ParticipantID))
	case errors.Is(err, webpush.ErrUnauthorized):
		// Our VAPID key or JWT, not the subscription: every push fails until the setup is fixed.
		s.log.Error("push_unauthorized", zap.String("room", b.room), zap.Int64("participant", b.sub.ParticipantID), zap.Error(err))
	case err != nil:
		s.log.Warn("push_failed", zap.String("room", b.room), zap.Int64("participant", b.sub.ParticipantID), zap.Error(err))
	default:
		s.log.Debug("push_sent", zap.String("room", b.room), zap.Int64("participant", b.sub.ParticipantID), zap.String("kind", b.kind), zap.Int("count", b.count))
	}
}

// pushTopic is a Topic header for room: at most 32 characters of the base64url alphabet.
func pushTopic(room string) string {
	t := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return -1
	}, room)
	return t[:min(len(t), 32)]
}

// newPushClient posts to push services: no redirect, which could lead the post to a host the
// allowlist refused.
func newPushClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
