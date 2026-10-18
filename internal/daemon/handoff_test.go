package daemon

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
)

// oneConnListener hands out one connection, then reports closed once Close is called.
type oneConnListener struct {
	conns  chan net.Conn
	closed chan struct{}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *oneConnListener) Close() error   { close(l.closed); return nil }
func (l *oneConnListener) Addr() net.Addr { return &net.UnixAddr{Name: "test", Net: "unix"} }

// ServeHandoff returns only once its in-flight answers have closed their connections: on Windows
// a pipe instance left open refuses the next listen on its name.
func TestServeHandoffWaitsForAnswers(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	ln := &oneConnListener{conns: make(chan net.Conn, 1), closed: make(chan struct{})}
	ln.conns <- server
	opened, unblock := make(chan struct{}), make(chan struct{})
	served := make(chan struct{})
	go func() {
		defer close(served)
		ServeHandoff(ln, func(string) (string, error) {
			close(opened)
			<-unblock
			return "http://127.0.0.1:1/", nil
		}, zap.NewNop())
	}()
	if _, err := client.Write([]byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("the answer never started")
	}
	_ = ln.Close()
	select {
	case <-served:
		t.Fatal("ServeHandoff returned with an answer in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(unblock)
	// The answer is written, then its connection closed: reading past the line hits EOF.
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 4096)
	for {
		if _, err := client.Read(buf); err != nil {
			if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("server connection not closed: %v", err)
		}
	}
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("ServeHandoff did not return once its answer was done")
	}
}
