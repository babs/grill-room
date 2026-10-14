package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// 005-AC20: X-Forwarded-For names the client only behind a trusted proxy, by its right-most
// untrusted hop.
func TestClientIP(t *testing.T) {
	proxies, err := ParseTrustedProxies("10.0.0.0/8, 192.0.2.7")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		trusted []netip.Prefix
		remote  string
		xff     []string
		want    string
	}{
		{"no list: the peer", nil, "10.0.0.5:5000", []string{"198.51.100.9"}, "10.0.0.5"},
		{"untrusted peer cannot spoof", proxies, "203.0.113.1:5000", []string{"198.51.100.9"}, "203.0.113.1"},
		{"private peer not listed cannot spoof", proxies, "172.16.0.1:5000", []string{"198.51.100.9"}, "172.16.0.1"},
		{"one proxy", proxies, "10.0.0.5:5000", []string{"198.51.100.9"}, "198.51.100.9"},
		{"client-written hops left of the real one", proxies, "10.0.0.5:5000", []string{"1.1.1.1, 198.51.100.9"}, "198.51.100.9"},
		{"chain of trusted proxies", proxies, "10.0.0.5:5000", []string{"198.51.100.9, 192.0.2.7, 10.1.2.3"}, "198.51.100.9"},
		{"header split over lines", proxies, "10.0.0.5:5000", []string{"1.1.1.1", "198.51.100.9, 10.9.9.9"}, "198.51.100.9"},
		{"garbage stops the walk", proxies, "10.0.0.5:5000", []string{"198.51.100.9, garbage, 10.1.2.3"}, "10.1.2.3"},
		{"no header", proxies, "10.0.0.5:5000", nil, "10.0.0.5"},
		{"mapped trusted peer, no header", proxies, "[::ffff:10.0.0.5]:5000", nil, "10.0.0.5"},
		{"all trusted", proxies, "10.0.0.5:5000", []string{"10.7.7.7"}, "10.7.7.7"},
		{"mapped v4 peer", proxies, "[::ffff:10.0.0.5]:5000", []string{"2001:db8::1"}, "2001:db8::1"},
	} {
		s := &Server{opts: Options{TrustedProxies: tc.trusted}}
		r := &http.Request{RemoteAddr: tc.remote, Header: http.Header{}}
		for _, v := range tc.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := s.clientIP(r); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	for _, bad := range []string{"10.0.0.0/33", "nope", "10.0.0.1/8/2"} {
		if _, err := ParseTrustedProxies(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if p, err := ParseTrustedProxies(" , "); err != nil || len(p) != 0 {
		t.Errorf("empty list: %v %v", p, err)
	}
}

// fromClient sends a JSON request as a client behind the proxy at 127.0.0.1.
func (e *env) fromClient(t *testing.T, method, path, token, ip, ua string, body any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(t.Context(), method, e.ts.URL+path, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	req.Header.Set("User-Agent", ua)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// 005-AC20: the super-admin sees each participant's IP and user agent at join and at last
// activity; members never do; the purge clears them past the retention, and the room's delete
// takes them along.
func TestParticipantClients(t *testing.T) {
	e := newEnv(t)
	clk := e.clock()
	room := e.createRoom(t, "R")
	inv := e.createInvite(t, room.ID)
	code, b := e.fromClient(t, "POST", "/api/join", "", "198.51.100.1", "JoinAgent/1", map[string]string{"invite": inv.Token, "name": "Alice"})
	if code != http.StatusCreated {
		t.Fatalf("join: %d %s", code, b)
	}
	alice := decodeJSON[joined](t, b)
	clients := func() adminParticipant {
		t.Helper()
		_, b := e.do(t, "GET", "/api/admin/rooms/"+room.ID+"/participants", e.admin, nil)
		ps := decodeJSON[[]adminParticipant](t, b)
		if len(ps) != 1 || ps[0].Client == nil {
			t.Fatalf("participants: %s", b)
		}
		return ps[0]
	}
	if c := clients().Client; c.JoinIP != "198.51.100.1" || c.JoinUA != "JoinAgent/1" || c.LastIP != "198.51.100.1" || c.LastSeen == nil {
		t.Fatalf("after join: %+v", c)
	}
	// Activity from another network shows as the last one; the join stays.
	clk.add(2 * time.Minute)
	if code, _ := e.fromClient(t, "GET", "/api/rooms/"+room.ID, alice.Token, "203.0.113.4", "Phone/2", nil); code != http.StatusOK {
		t.Fatal("get room")
	}
	if c := clients().Client; c.JoinIP != "198.51.100.1" || c.LastIP != "203.0.113.4" || c.LastUA != "Phone/2" {
		t.Fatalf("after activity: %+v", c)
	}
	// A member's views carry none of it: the room and the agent state the details card reads (009-AC36).
	for _, path := range []string{"", "/agent"} {
		_, b = e.do(t, "GET", "/api/rooms/"+room.ID+path, alice.Token, nil)
		for _, leak := range []string{"198.51.100.1", "203.0.113.4", "Phone/2", "JoinAgent"} {
			if strings.Contains(string(b), leak) {
				t.Fatalf("member view %q leaks %q: %s", path, leak, b)
			}
		}
	}
	// Within the retention the purge keeps them; past it, it clears both.
	if n, err := e.st.PurgeClients(t.Context(), clk.now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("early purge: %d %v", n, err)
	}
	if n, err := e.st.PurgeClients(t.Context(), time.Now().Add(time.Hour)); err != nil || n != 2 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if c := clients().Client; c.JoinIP != "" || c.JoinUA != "" || c.LastIP != "" || c.LastUA != "" {
		t.Fatalf("after purge: %+v", c)
	}
}
