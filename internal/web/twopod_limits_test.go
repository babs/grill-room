package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// newRelayPair is two pods on one database sharing one fake relay.
func newRelayPair(t *testing.T) (a, b *env, rl *relay) {
	t.Helper()
	rl = &relay{box: mailbox{ch: make(chan [3]string, 16)}}
	opts := testOptions
	opts.MagicLinkTTL = 15 * time.Minute
	opts.SendMail = func(ctx context.Context, to, subject, body string) error {
		if rl.fail.Load() {
			return errors.New("dial: connection refused")
		}
		return rl.box.send(ctx, to, subject, body)
	}
	a, b = newPair(t, opts)
	return a, b, rl
}

// 017-AC16: the 11th failed admin login from one IP in the window gets 429 whichever pod takes it,
// the attempts alternating pods.
func TestTwoPodsAdminLoginLimit(t *testing.T) {
	a, b := newPair(t, testOptions)
	pods := []*device{a.device(t), b.device(t)}
	for i := range loginAttempts {
		if code, _ := pods[i%2].login(t, "guess-"+strconv.Itoa(i), "203.0.113.9"); code != http.StatusUnauthorized {
			t.Fatalf("guess %d: %d", i, code)
		}
	}
	for i, d := range pods {
		if code, _ := d.login(t, "guess", "203.0.113.9"); code != http.StatusTooManyRequests {
			t.Fatalf("pod %d past the limit: %d", i, code)
		}
		if code, _ := d.login(t, a.admin, "203.0.113.9"); code != http.StatusTooManyRequests {
			t.Fatalf("pod %d, right token past the limit: %d", i, code)
		}
	}
	if code, _ := pods[1].login(t, a.admin, "203.0.113.10"); code != http.StatusCreated {
		t.Fatalf("other IP: %d", code)
	}
}

// 017-AC16: each magic link cap of 011 holds for the cluster, the requests alternating pods.
func TestTwoPodsMagicLinkLimits(t *testing.T) {
	a, b, _ := newRelayPair(t)
	pods := []*device{a.device(t), b.device(t)}
	for _, c := range []struct {
		name  string
		cap   int
		email func(i int) string
		ip    func(i int) string
	}{
		{"per ip", magicPerIP, func(i int) string { return "ip" + strconv.Itoa(i) + "@example.com" }, func(int) string { return "198.51.100.1" }},
		{"per address from one ip", magicPerAddressIP, func(int) string { return "one@example.com" }, func(int) string { return "198.51.100.2" }},
		{"per address", magicPerAddress, func(int) string { return "all@example.com" }, func(i int) string { return "203.0.113." + strconv.Itoa(10+i) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			ask := func(d *device, i int) int {
				code, _ := d.do(t, "POST", "/api/profile/signin", map[string]string{"email": c.email(i)}, "X-Forwarded-For", c.ip(i))
				return code
			}
			for i := range c.cap {
				if code := ask(pods[i%2], i); code != http.StatusAccepted {
					t.Fatalf("request %d: %d", i, code)
				}
			}
			for i, d := range pods {
				if code := ask(d, c.cap+i); code != http.StatusTooManyRequests {
					t.Fatalf("pod %d past the cap: %d", i, code)
				}
			}
		})
	}
}

// 017 I25, 011-AC28: a bind send failing on pod A makes pod B answer sign-in "mail failed"; a
// bind send going out on B clears it for A.
func TestTwoPodsMailFailing(t *testing.T) {
	a, b, rl := newRelayPair(t)
	alice := a.device(t)
	alice.join(t, a.createRoom(t, "A").ID, "Alice")
	aliceOnB := &device{e: b, c: alice.c}
	rl.fail.Store(true)
	if code, out := alice.do(t, "POST", "/api/profile/email", map[string]string{"email": "alice@example.com"}); code != http.StatusBadGateway {
		t.Fatalf("bind on a failing relay: %d %s", code, out)
	}
	if code, _, m := post(t, b, "/api/profile/signin", `{"email":"nobody@example.com"}`); code != http.StatusServiceUnavailable || m["reason"] != "mail_failed" {
		t.Fatalf("sign-in on B: %d %v", code, m)
	}
	rl.fail.Store(false)
	if code, out := aliceOnB.do(t, "POST", "/api/profile/email", map[string]string{"email": "alice@example.com"}); code != http.StatusAccepted {
		t.Fatalf("bind on B once the relay works: %d %s", code, out)
	}
	rl.box.next(t)
	if code, _, m := post(t, a, "/api/profile/signin", `{"email":"nobody@example.com"}`); code != http.StatusAccepted {
		t.Fatalf("sign-in on A after B's send: %d %v", code, m)
	}
}
