package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
)

// ErrLocked means another supervisor holds the lock on this state dir.
var ErrLocked = errors.New("locked")

// LockSupervisor makes this process the state dir's only supervisor (003-AC6): it fails with
// ErrLocked while another one runs.
func LockSupervisor(stateDir string) (release func(), err error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	return lockFile(filepath.Join(stateDir, "supervisor.lock"))
}

// handoff is a launch's request to the running supervisor, and its answer.
type handoff struct {
	Room  string `json:"room,omitempty"`
	Link  string `json:"link,omitempty"`
	Error string `json:"error,omitempty"`
}

// ListenHandoff opens the local socket (a named pipe on Windows) later launches talk to.
func ListenHandoff(ctx context.Context, stateDir string) (net.Listener, error) {
	return listenLocal(ctx, stateDir)
}

// ServeHandoff answers launches until ln closes. open receives the room a launch hands over, ""
// for none, and returns a fresh link to the admin page.
func ServeHandoff(ln net.Listener, open func(room string) (string, error), log *zap.Logger) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			var req handoff
			line, err := bufio.NewReader(io.LimitReader(c, 64<<10)).ReadBytes('\n')
			if err == nil {
				err = json.Unmarshal(line, &req)
			}
			var resp handoff
			if err != nil {
				resp.Error = "bad handoff request"
			} else if resp.Link, err = open(req.Room); err != nil {
				resp.Error = err.Error()
			}
			log.Info("launch_handed_over", zap.Bool("room", req.Room != ""), zap.String("error", resp.Error))
			b, _ := json.Marshal(resp)
			_, _ = c.Write(append(b, '\n'))
		}()
	}
}

// Handoff gives room, "" for none, to the supervisor running on stateDir and returns its admin
// page link. The supervisor may still be starting: the lock comes before the socket.
func Handoff(ctx context.Context, stateDir, room string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var c net.Conn
	var err error
	for {
		if c, err = dialLocal(ctx, stateDir); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("the running supervisor does not answer on its socket: %w", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer func() { _ = c.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	b, _ := json.Marshal(handoff{Room: room})
	if _, err := c.Write(append(b, '\n')); err != nil {
		return "", fmt.Errorf("hand over: %w", err)
	}
	line, err := bufio.NewReader(io.LimitReader(c, 64<<10)).ReadBytes('\n')
	if err != nil {
		return "", fmt.Errorf("hand over: %w", err)
	}
	var resp handoff
	if err := json.Unmarshal(line, &resp); err != nil {
		return "", fmt.Errorf("hand over: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("the running supervisor: %s", resp.Error)
	}
	// Security: only a link to a local admin page is opened in the browser.
	if !strings.HasPrefix(resp.Link, "http://127.0.0.1:") {
		return "", fmt.Errorf("the running supervisor answered an unexpected link")
	}
	return resp.Link, nil
}
