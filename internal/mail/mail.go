// Package mail sends plain-text mail over SMTP.
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// SMTP is an SMTP relay. STARTTLS is used whenever the relay offers it, and required before AUTH
// unless the relay is on localhost.
type SMTP struct {
	// Addr is host:port.
	Addr string
	// From is the sender address, e.g. "grill-room <noreply@example.com>".
	From     string
	Username string
	Password string
}

// Validate rejects a relay the server cannot send through.
func (c SMTP) Validate() error {
	if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		return fmt.Errorf("address %q: want host:port", c.Addr)
	}
	if _, err := mail.ParseAddress(c.From); err != nil {
		return fmt.Errorf("from %q: %w", c.From, err)
	}
	if (c.Username == "") != (c.Password == "") {
		return errors.New("username and password go together")
	}
	return nil
}

// Send mails a plain-text body to one recipient. ctx bounds the whole exchange.
func (c SMTP) Send(ctx context.Context, to, subject, body string) error {
	from, err := mail.ParseAddress(c.From)
	if err != nil {
		return fmt.Errorf("from: %w", err)
	}
	rcpt, err := mail.ParseAddress(to)
	if err != nil {
		return fmt.Errorf("to: %w", err)
	}
	// Security: a CR or LF would let a subject inject headers.
	if strings.ContainsAny(subject, "\r\n") {
		return errors.New("subject holds a line break")
	}
	host, _, _ := net.SplitHostPort(c.Addr)
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.Addr, err)
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			return fmt.Errorf("deadline: %w", err)
		}
	}
	cl, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("smtp hello: %w", err)
	}
	defer func() { _ = cl.Close() }()
	if ok, _ := cl.Extension("STARTTLS"); ok {
		if err := cl.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if c.Username != "" {
		// smtp.PlainAuth refuses to send the password over a connection without TLS.
		if err := cl.Auth(smtp.PlainAuth("", c.Username, c.Password, host)); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}
	if err := cl.Mail(from.Address); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	if err := cl.Rcpt(rcpt.Address); err != nil {
		return fmt.Errorf("rcpt to: %w", err)
	}
	w, err := cl.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := w.Write(message(from, rcpt, subject, body, time.Now())); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("data end: %w", err)
	}
	return cl.Quit()
}

func message(from, to *mail.Address, subject, body string, now time.Time) []byte {
	var b strings.Builder
	for _, h := range [][2]string{
		{"From", from.String()},
		{"To", to.String()},
		{"Subject", mime.QEncoding.Encode("utf-8", subject)},
		{"Date", now.Format(time.RFC1123Z)},
		{"MIME-Version", "1.0"},
		{"Content-Type", "text/plain; charset=utf-8"},
		{"Content-Transfer-Encoding", "8bit"},
	} {
		b.WriteString(h[0] + ": " + h[1] + "\r\n")
	}
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	return []byte(b.String())
}
