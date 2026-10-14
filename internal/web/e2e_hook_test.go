//go:build e2e

package web

import (
	"net/http"
	"testing"

	"github.com/babs/grill-room/internal/store"
)

// 011-AC16: the e2e build's hook runs one purge pass with the threshold asked, for the admin only,
// restricted to profile_ids when given.
func TestE2EPurgeHook(t *testing.T) {
	e := newEnv(t)
	d, mine := e.ghost(t)
	_, other := e.ghost(t)
	uuids := map[int64]string{}
	_, list := e.do(t, "GET", "/api/profiles", e.admin, nil)
	for _, p := range decodeJSON[[]store.AdminProfile](t, list) {
		uuids[p.ID] = p.UUID
	}
	purge := func(body map[string]any) (int, []string) {
		t.Helper()
		code, b := e.do(t, "POST", "/api/admin/test/purge", e.admin, body)
		if code != http.StatusOK {
			return code, nil
		}
		return code, decodeJSON[map[string][]string](t, b)["deleted"]
	}
	if code, _ := d.do(t, "POST", "/api/admin/test/purge", map[string]int{"after_s": 0}); code != http.StatusUnauthorized {
		t.Fatalf("hook without the admin: %d", code)
	}
	if code, got := purge(map[string]any{"after_s": 3600}); code != http.StatusOK || got == nil || len(got) != 0 {
		t.Fatalf("within the threshold: %d %v", code, got)
	}
	if code, got := purge(map[string]any{"after_s": 0, "profile_ids": []int64{mine}}); code != http.StatusOK || len(got) != 1 || got[0] != uuids[mine] {
		t.Fatalf("restricted: %d %v, want only %s", code, got, uuids[mine])
	}
	if code, got := purge(map[string]any{"after_s": 0}); code != http.StatusOK || len(got) != 1 || got[0] != uuids[other] {
		t.Fatalf("unrestricted: %d %v, want %s", code, got, uuids[other])
	}
}
