//go:build !e2e

package web

import (
	"net/http"
	"testing"
)

// 011-AC16: the release build has no purge hook, whoever asks.
func TestNoE2EPurgeHook(t *testing.T) {
	e := newEnv(t)
	if code, _ := e.do(t, "POST", "/api/admin/test/purge", e.admin, map[string]int{"after_s": 0}); code != http.StatusNotFound {
		t.Fatalf("purge hook in a normal build: %d", code)
	}
}
