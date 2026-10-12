package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	// DefaultClientRetention is how long a participant's IP and user agent are kept (005-AC20).
	DefaultClientRetention = 30 * 24 * time.Hour
	// clientWriteEvery throttles the last-activity write of a participant whose IP and agent did
	// not change.
	clientWriteEvery = time.Minute
	// maxClientsTracked is where the throttle map sweeps the entries it no longer needs.
	maxClientsTracked = 10000
)

// ParseTrustedProxies reads a comma-separated list of CIDRs or bare addresses.
func ParseTrustedProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for f := range strings.SplitSeq(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			a, aerr := netip.ParseAddr(f)
			if aerr != nil {
				return nil, fmt.Errorf("trusted proxy %q: want a CIDR or an address", f)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func (s *Server) trusted(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range s.opts.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// clientIP is the address of the client (005-AC20). X-Forwarded-For counts only when the direct
// peer is a trusted proxy; then the client is the right-most hop that is not one, since every
// hop left of the last untrusted one is the client's to write.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !s.trusted(peer) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	ip := peer.Unmap()
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break // a hop no proxy of ours wrote: what is left of it is untrustworthy too
		}
		ip = a.Unmap()
		if !s.trusted(a) {
			break
		}
	}
	return ip.String()
}

// clientSeen is the last IP and user agent stored for a participant, and when.
type clientSeen struct {
	ip, ua string
	at     time.Time
}

// clients throttles the last-activity writes: one per participant per clientWriteEvery, unless
// the IP or user agent changed.
type clients struct {
	mu sync.Mutex
	m  map[int64]clientSeen
}

// userAgent is r's user agent as stored, cut like an admin session's.
func userAgent(r *http.Request) string { return clip(r.UserAgent(), maxUserAgent) }

// seen records participant pid's activity: its IP and user agent, and when (005-AC20, 005-AC6).
// Best effort: a failed write costs the admin view one update, never the request.
func (s *Server) seen(ctx context.Context, pid int64, r *http.Request) {
	now, ip, ua := s.now(), s.clientIP(r), userAgent(r)
	s.clients.mu.Lock()
	last, ok := s.clients.m[pid]
	if ok && last.ip == ip && last.ua == ua && now.Sub(last.at) < clientWriteEvery {
		s.clients.mu.Unlock()
		return
	}
	if len(s.clients.m) >= maxClientsTracked {
		for id, c := range s.clients.m {
			if now.Sub(c.at) >= clientWriteEvery {
				delete(s.clients.m, id)
			}
		}
	}
	s.clients.m[pid] = clientSeen{ip: ip, ua: ua, at: now}
	s.clients.mu.Unlock()
	if err := s.bus.Store.ParticipantSeen(ctx, pid, ip, ua, now); err != nil && ctx.Err() == nil {
		s.log.Warn("participant_seen_failed", zap.Int64("participant", pid), zap.Error(err))
	}
}
