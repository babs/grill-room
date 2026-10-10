package livekit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const secret = "0123456789abcdef0123456789abcdef"

// verify checks an HS256 token's signature and returns its claims.
func verify(t *testing.T, tok, secret string) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		t.Fatal("bad signature")
	}
	var header, claims map[string]any
	for i, v := range []*map[string]any{&header, &claims} {
		b, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatal(err)
		}
	}
	if header["alg"] != "HS256" {
		t.Fatalf("alg %v", header["alg"])
	}
	return claims
}

func TestToken(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	yes := true
	tok, err := Token("APIkey", secret, "42", "Alice", Grant{Room: "r1", RoomJoin: true, CanPublish: &yes, CanPublishData: new(bool), CanPublishSources: []string{"microphone"}}, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	c := verify(t, tok, secret)
	if c["iss"] != "APIkey" || c["sub"] != "42" || c["name"] != "Alice" {
		t.Fatalf("claims %v", c)
	}
	if c["exp"].(float64) != float64(now.Add(time.Hour).Unix()) || c["nbf"].(float64) > float64(now.Unix()) {
		t.Fatalf("validity %v %v", c["nbf"], c["exp"])
	}
	v := c["video"].(map[string]any)
	// canPublishData false must be sent: an absent field means LiveKit's default, true.
	if v["room"] != "r1" || v["roomJoin"] != true || v["canPublish"] != true || v["canPublishData"] != false {
		t.Fatalf("video grant %v", v)
	}
	if _, ok := v["roomAdmin"]; ok {
		t.Fatalf("participant token carries roomAdmin: %v", v)
	}
	if src := v["canPublishSources"].([]any); len(src) != 1 || src[0] != "microphone" {
		t.Fatalf("sources %v", src)
	}
	if _, err := Token("k", "other secret, 32 characters long!", "42", "", Grant{}, time.Hour, now); err != nil {
		t.Fatal(err)
	}
}

func TestClient(t *testing.T) {
	type call struct {
		path  string
		body  map[string]string
		video map[string]any
	}
	var calls []call
	status := http.StatusOK
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]string
		_ = json.Unmarshal(b, &body)
		c := verify(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), secret)
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		calls = append(calls, call{r.URL.Path, body, c["video"].(map[string]any)})
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"code":"x","msg":"boom"}`)
	}))
	defer ts.Close()
	c := &Client{URL: ts.URL + "/", Key: "k", Secret: secret, HTTP: ts.Client()}

	if err := c.RemoveParticipant(t.Context(), "r1", "42"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteRoom(t.Context(), "r1"); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		path  string
		grant string
	}{{"/twirp/livekit.RoomService/RemoveParticipant", "roomAdmin"}, {"/twirp/livekit.RoomService/DeleteRoom", "roomCreate"}}
	for i, w := range want {
		if calls[i].path != w.path || calls[i].body["room"] != "r1" || calls[i].video[w.grant] != true || calls[i].video["room"] != "r1" {
			t.Fatalf("call %d: %+v", i, calls[i])
		}
	}
	if calls[0].body["identity"] != "42" {
		t.Fatalf("identity %+v", calls[0].body)
	}

	status = http.StatusNotFound // already gone
	if err := c.DeleteRoom(t.Context(), "r1"); err != nil {
		t.Fatalf("404: %v", err)
	}
	for _, code := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
		status = code
		if err := c.RemoveParticipant(t.Context(), "r1", "42"); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("HTTP %d: %v", code, err)
		}
	}
	ts.Close()
	if err := c.DeleteRoom(t.Context(), "r1"); err == nil {
		t.Fatal("unreachable LiveKit: no error")
	}
}
