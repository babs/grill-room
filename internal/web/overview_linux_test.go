package web

import (
	"context"
	"net/http"
	"os"
	"syscall"
	"testing"
)

// 004-AC5: the instance view reports the version, the database's size and its disk's free space,
// STT, the connected daemons by version, and whether SMTP is set.
func TestInstanceHealth(t *testing.T) {
	e := newEnv(t)
	instance := func() instanceResp {
		t.Helper()
		code, b := e.do(t, "GET", "/api/admin/instance", e.admin, nil)
		if code != http.StatusOK {
			t.Fatalf("instance: %d %s", code, b)
		}
		return decodeJSON[instanceResp](t, b)
	}
	if got := instance(); got.Version != "" || got.DBBytes != 0 || got.DiskFree != -1 || got.SMTP || got.STT.Voice || got.Daemons.Connected != 0 {
		t.Fatalf("bare instance: %+v", got)
	}

	ve, _ := newVoiceEnv(t)
	ve.srv.opts.Version, ve.srv.opts.DBPath, ve.srv.opts.SendMail = "v1.2.3", ve.dbPath, func(context.Context, string, string, string) error { return nil }
	room, alice := ve.member(t)
	ve.uploadDoc(t, room.ID, alice.Token, "plan.md", md(5000))
	if code, _ := ve.do(t, "POST", "/api/stt/heartbeat", sttToken, map[string]any{"worker": "w1", "rooms": []string{room.ID}}); code != http.StatusOK {
		t.Fatal("heartbeat")
	}
	for i, v := range []string{"v0.0.3", "v0.0.3", "v0.0.4"} {
		d := ve.daemonFor(t, room.ID, "D"+itoa(int64(i)))
		d.events, d.drop = ve.openStream(t, "/api/rooms/"+room.ID+"/daemon/events", d.tok, "")
		next(t, d.events)
		if code, _ := ve.do(t, "POST", "/api/rooms/"+room.ID+"/daemon/status", d.tok, map[string]any{"can_host": true, "version": v}); code != http.StatusNoContent {
			t.Fatal("status")
		}
	}
	code, b := ve.do(t, "GET", "/api/admin/instance", ve.admin, nil)
	if code != http.StatusOK {
		t.Fatalf("instance: %d %s", code, b)
	}
	got := decodeJSON[instanceResp](t, b)
	var size int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(ve.dbPath + suffix); err == nil {
			size += fi.Size()
		}
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(ve.dbPath, &st); err != nil {
		t.Fatal(err)
	}
	free := int64(st.Bavail) * st.Bsize
	if got.Version != "v1.2.3" || got.DBBytes < 5000 || got.DBBytes != size || !got.SMTP || !got.STT.Voice || !got.STT.Configured || got.STT.Workers != 1 ||
		got.Daemons.Connected != 3 || len(got.Daemons.Versions) != 2 || got.Daemons.Versions["v0.0.3"] != 2 || got.Daemons.Versions["v0.0.4"] != 1 ||
		got.DiskTotal < got.DiskFree || abs(got.DiskFree-free) > 64<<20 {
		t.Fatalf("instance: %+v, db files %d, free %d", got, size, free)
	}
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
