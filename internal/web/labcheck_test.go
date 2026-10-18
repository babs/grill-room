package web

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// labCheck runs tools/lab-check.sh against the test server with env added, and returns its output.
func (e *env) labCheck(t *testing.T, env ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "../../tools/lab-check.sh")
	cmd.Env = append(os.Environ(), append([]string{"GRILL_ROOM_URL=" + e.ts.URL, "GRILL_ROOM_ADMIN_TOKEN=" + e.admin}, env...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("lab-check: %v\n%s", err, stderr.String())
	}
	return string(out)
}

// 017-AC9: the cutover's check reads the server through its admin API and prints the same summary
// on every run against an unchanged server; the member and profile probes pass.
func TestLabCheck(t *testing.T) {
	for _, bin := range []string{"bash", "curl", "jq"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s missing", bin)
		}
	}
	e := newEnv(t)
	r1, r2 := e.createRoom(t, "Pricing review"), e.createRoom(t, "Café")
	d := e.device(t)
	d.join(t, r1.ID, "Alice")
	e.join(t, e.createInvite(t, r2.ID).Token, "Bob")
	u, err := url.Parse(e.ts.URL + "/api/rooms/" + r1.ID)
	if err != nil {
		t.Fatal(err)
	}
	var roomCookie, session string
	for _, c := range d.c.Jar.Cookies(u) {
		switch c.Name {
		case cookieName:
			roomCookie = c.Value
		case profileCookie:
			session = c.Value
		}
	}
	if roomCookie == "" || session == "" {
		t.Fatalf("the join set no room or profile cookie: %v", d.c.Jar.Cookies(u))
	}
	probes := []string{"GRILL_ROOM_CHECK_ROOM=" + r1.ID, "GRILL_ROOM_CHECK_COOKIE=" + roomCookie, "GRILL_ROOM_CHECK_SESSION=" + session}
	first, second := e.labCheck(t, probes...), e.labCheck(t, probes...)
	if first != second {
		t.Fatalf("two runs differ:\n%s\n---\n%s", first, second)
	}
	for _, want := range []string{
		`{"created_at":`, `"room":"` + r1.ID + `"`, `"room":"` + r2.ID + `"`, `"name":"Alice"`, `"name":"Bob"`,
		`{"pronunciations":`, `{"probe":"room","room":"` + r1.ID + `","same_room":true,"status":200}`, `{"probe":"profile","profile":`,
	} {
		if !strings.Contains(first, want) {
			t.Errorf("output lacks %s:\n%s", want, first)
		}
	}
	if strings.Contains(first, e.admin) || strings.Contains(first, session) || strings.Contains(first, roomCookie) {
		t.Errorf("output holds a secret:\n%s", first)
	}
}
