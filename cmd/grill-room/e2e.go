//go:build e2e

package main

import (
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"sync"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/plugin"
)

// e2eGitHost is the name the browser tests' git server answers to: httptest's certificate holds it.
const e2eGitHost = "example.com"

// In the e2e build only, GRILL_ROOM_TEST_GIT_ALLOW_LOOPBACK=1 sends plugin fetches for
// example.com to the local git server whose CA GRILL_ROOM_TEST_GIT_CA names.
func init() {
	testPluginClient = func(log *zap.Logger) *http.Client {
		if os.Getenv("GRILL_ROOM_TEST_GIT_ALLOW_LOOPBACK") != "1" {
			return nil
		}
		ca := os.Getenv("GRILL_ROOM_TEST_GIT_CA")
		log.Warn("plugin_fetch_loopback", zap.String("host", e2eGitHost), zap.String("ca", ca))
		return &http.Client{Timeout: plugin.Timeout, Transport: &lazyCA{path: ca}}
	}
}

// lazyCA builds the client on the first request: the git server writes its CA as the tests start,
// beside this server.
type lazyCA struct {
	path string
	once sync.Once
	rt   http.RoundTripper
	err  error
}

func (l *lazyCA) RoundTrip(req *http.Request) (*http.Response, error) {
	l.once.Do(func() {
		pem, err := os.ReadFile(l.path) //nolint:gosec // the test run's own file, named by its config
		if err != nil {
			l.err = err
			return
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			l.err = errors.New("no certificate in " + l.path)
			return
		}
		l.rt = plugin.NewE2EClient(e2eGitHost, pool).Transport
	})
	if l.err != nil {
		return nil, l.err
	}
	return l.rt.RoundTrip(req)
}
