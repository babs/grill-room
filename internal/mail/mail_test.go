package mail

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeRelay answers one SMTP session without STARTTLS or AUTH and returns what DATA carried.
func fakeRelay(t *testing.T) (string, <-chan string) {
	t.Helper()
	return fakeRelayWith(t, "127.0.0.1", "250 fake")
}

// fakeRelayWith listens on host and answers EHLO with ehlo, which may advertise extensions.
func fakeRelayWith(t *testing.T, host, ehlo string) (string, <-chan string) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", host+":0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan string, 2)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
		say("220 fake")
		var data strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"):
				say(ehlo)
			case strings.HasPrefix(cmd, "AUTH"):
				got <- "AUTH SENT"
				say("235 ok")
			case cmd == "DATA":
				say("354 go")
				for {
					l, err := r.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
					data.WriteString(l)
				}
				got <- data.String()
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().String(), got
}

func TestSend(t *testing.T) {
	addr, got := fakeRelay(t)
	c := SMTP{Addr: addr, From: "grill-room <noreply@example.com>"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := c.Send(ctx, "alice@example.com", "Sign in à grill-room", "Open:\nhttps://x/profile#magic=abc\n.dot line"); err != nil {
		t.Fatal(err)
	}
	msg := <-got
	for _, want := range []string{
		"From: \"grill-room\" <noreply@example.com>\r\n",
		"To: <alice@example.com>\r\n",
		"Subject: =?utf-8?q?Sign_in_=C3=A0_grill-room?=\r\n",
		"Content-Type: text/plain; charset=utf-8\r\n",
		"\r\n\r\nOpen:\r\nhttps://x/profile#magic=abc\r\n..dot line",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
}

func TestSendRefuses(t *testing.T) {
	c := SMTP{Addr: "127.0.0.1:1", From: "noreply@example.com"}
	if err := c.Send(t.Context(), "a@example.com", "x\r\nBcc: evil@example.com", "b"); err == nil {
		t.Error("header injection through the subject")
	}
	if err := c.Send(t.Context(), "not an address", "x", "b"); err == nil {
		t.Error("bad recipient accepted")
	}
	// A relay that accepts and never greets must not hold the sender past its deadline.
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			time.Sleep(2 * time.Second) // a server slower than the client's 200 ms deadline: no assertion on it
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := (SMTP{Addr: ln.Addr().String(), From: "noreply@example.com"}).Send(ctx, "a@example.com", "x", "b"); err == nil || time.Since(start) > time.Second {
		t.Errorf("silent relay: %v after %s", err, time.Since(start))
	}
}

func TestValidate(t *testing.T) {
	for name, c := range map[string]SMTP{
		"no port":       {Addr: "mail.example.com", From: "a@example.com"},
		"bad from":      {Addr: "mail.example.com:25", From: "nobody"},
		"user, no pass": {Addr: "mail.example.com:25", From: "a@example.com", Username: "u"},
	} {
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Security: a relay offering AUTH without STARTTLS never gets the password.
func TestSendRefusesAuthWithoutTLS(t *testing.T) {
	// net/smtp lets AUTH through in clear to 127.0.0.1 and localhost only: 127.0.0.2 stands for a remote relay.
	addr, got := fakeRelayWith(t, "127.0.0.2", "250-fake\r\n250 AUTH PLAIN")
	c := SMTP{Addr: addr, From: "noreply@example.com", Username: "u", Password: "p"}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := c.Send(ctx, "a@example.com", "x", "b"); err == nil || !strings.Contains(err.Error(), "auth") {
		t.Fatalf("got %v, want an auth error", err)
	}
	select {
	case s := <-got:
		t.Fatalf("relay received %q", s)
	case <-time.After(100 * time.Millisecond):
	}
}

// refusingRelay answers one SMTP session, replying reply at step: MAIL, RCPT, DATA or END (the
// data's final dot).
func refusingRelay(t *testing.T, step, reply string) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
		at := func(s, ok string) {
			if s == step {
				say(reply)
			} else {
				say(ok)
			}
		}
		say("220 fake")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "MAIL"):
				at("MAIL", "250 ok")
			case strings.HasPrefix(cmd, "RCPT"):
				at("RCPT", "250 ok")
			case cmd == "DATA":
				if step == "DATA" {
					say(reply)
					continue
				}
				say("354 go")
				for {
					if l, err := r.ReadString('\n'); err != nil || l == ".\r\n" {
						break
					}
				}
				at("END", "250 queued")
			case cmd == "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().String()
}

// 011-AC28: only a refusal of the recipient is ErrRecipient; policy, auth, the sender or a
// temporary failure are the relay's problem.
func TestSendRecipientRefusal(t *testing.T) {
	for _, c := range []struct {
		step, reply string
		recipient   bool
	}{
		{"RCPT", "550 5.1.1 no such user", true},
		{"RCPT", "553 mailbox name not allowed", true},
		{"RCPT", "551 user not local", true},
		{"RCPT", "554 5.1.2 bad destination domain", true},
		{"END", "550 5.1.1 no such user", true},
		{"RCPT", "550 5.7.1 relaying denied", false},
		{"RCPT", "530 5.7.0 authentication required", false},
		{"RCPT", "530 authentication required", false},
		{"RCPT", "554 transaction failed", false},
		{"DATA", "554 5.7.1 rejected", false},
		{"END", "550 5.7.1 content refused", false},
		{"RCPT", "451 4.3.0 try later", false},
		{"MAIL", "550 5.1.8 sender refused", false},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		err := SMTP{Addr: refusingRelay(t, c.step, c.reply), From: "noreply@example.com"}.Send(ctx, "a@example.com", "x", "b")
		cancel()
		if err == nil || errors.Is(err, ErrRecipient) != c.recipient {
			t.Errorf("%s %q: %v, recipient refusal %v", c.step, c.reply, err, c.recipient)
		}
	}
}
