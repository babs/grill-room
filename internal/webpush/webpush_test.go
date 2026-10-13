package webpush

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The example of RFC 8291 section 5, with the intermediate values of its appendix A.
const (
	rfcPlaintext = "When I grow up, I want to be a watermelon"
	rfcASPrivate = "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"
	rfcUAPublic  = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	rfcUAPrivate = "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"
	rfcSalt      = "DGv6ra1nlYgDCS1FRnbzlw"
	rfcAuth      = "BTBZMqHH6r4Tts7J_aSIgg"
	rfcBody      = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml" +
		"mlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPT" +
		"pK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
)

func must(t *testing.T, s string) []byte {
	t.Helper()
	b, err := b64.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func rfcSub() *Subscription {
	s := &Subscription{Endpoint: "https://push.example.net/push/JzLQ3raZJfFBR0aqvOMsLrt54w4rJUsV"}
	s.Keys.P256dh, s.Keys.Auth = rfcUAPublic, rfcAuth
	return s
}

// 005-AC24: the encryption reproduces RFC 8291's example byte for byte.
func TestEncryptMatchesRFC8291(t *testing.T) {
	ua, err := rfcSub().keys()
	if err != nil {
		t.Fatal(err)
	}
	as, err := ecdh.P256().NewPrivateKey(must(t, rfcASPrivate))
	if err != nil {
		t.Fatal(err)
	}
	got, err := encrypt(ua, []byte(rfcPlaintext), as, must(t, rfcSalt))
	if err != nil {
		t.Fatal(err)
	}
	if want := must(t, rfcBody); !bytes.Equal(got, want) {
		t.Fatalf("body\n got %s\nwant %s", b64.EncodeToString(got), rfcBody)
	}
}

// decrypt runs Decrypt, the user agent's side, written apart from encrypt: a random key pair and
// salt must still decrypt.
func decrypt(t *testing.T, uaPriv *ecdh.PrivateKey, auth, body []byte) []byte {
	t.Helper()
	if string(body[16:20]) != "\x00\x00\x10\x00" {
		t.Fatalf("record size %x", body[16:20])
	}
	p, err := Decrypt(uaPriv, auth, body)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// 005-AC24: a fresh key pair and salt each time, which the user agent decrypts; a payload past
// one record is refused.
func TestEncryptRoundTrip(t *testing.T) {
	uaPriv, err := ecdh.P256().NewPrivateKey(must(t, rfcUAPrivate))
	if err != nil {
		t.Fatal(err)
	}
	a, err := Encrypt(rfcSub(), []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Encrypt(rfcSub(), []byte("hello"))
	if bytes.Equal(a[:16], b[:16]) || bytes.Equal(a[21:86], b[21:86]) {
		t.Fatal("salt or sender key reused")
	}
	if got := decrypt(t, uaPriv, must(t, rfcAuth), a); string(got) != "hello" {
		t.Fatalf("decrypted %q", got)
	}
	if full, err := Encrypt(rfcSub(), make([]byte, MaxPayload)); err != nil || len(full) != 4096 || MaxPayload != 3993 {
		t.Fatalf("a full body: %d octets, %v", len(full), err)
	}
	if _, err := Encrypt(rfcSub(), make([]byte, MaxPayload+1)); err == nil {
		t.Fatal("a payload past one record went through")
	}
}

// A subscription is checked as it is stored: https endpoint, a point on P-256, a 16-octet secret.
func TestSubscriptionCheck(t *testing.T) {
	ok := rfcSub()
	if err := ok.Check(); err != nil {
		t.Fatal(err)
	}
	offCurve := must(t, rfcUAPublic)
	offCurve[64] ^= 1
	for name, edit := range map[string]func(s *Subscription){
		"http":      func(s *Subscription) { s.Endpoint = "http://push.example.net/x" },
		"no host":   func(s *Subscription) { s.Endpoint = "https:///x" },
		"long":      func(s *Subscription) { s.Endpoint = "https://p.example/" + strings.Repeat("a", 2048) },
		"not b64":   func(s *Subscription) { s.Keys.P256dh = "!!" },
		"off curve": func(s *Subscription) { s.Keys.P256dh = b64.EncodeToString(offCurve) },
		"compressed": func(s *Subscription) {
			s.Keys.P256dh = b64.EncodeToString(append([]byte{2}, must(t, rfcUAPublic)[1:33]...))
		},
		"short auth": func(s *Subscription) { s.Keys.Auth = b64.EncodeToString(make([]byte, 15)) },
	} {
		s := rfcSub()
		edit(s)
		if err := s.Check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// 005-AC24: the VAPID header carries an ES256 JWT for the endpoint's origin, which the public key
// it names verifies (RFC 8292).
func TestVAPIDAuthorization(t *testing.T) {
	raw, err := NewVAPIDKey()
	if err != nil {
		t.Fatal(err)
	}
	v, err := ParseVAPID(raw, "mailto:ops@example.org")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	h, err := v.Authorization("https://push.example.net:8443/push/abc?x=1", now)
	if err != nil {
		t.Fatal(err)
	}
	tok, k, ok := strings.Cut(strings.TrimPrefix(h, "vapid t="), ", k=")
	if !ok || !strings.HasPrefix(h, "vapid t=") || k != v.PublicKey() {
		t.Fatalf("header %q", h)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt %q", tok)
	}
	var head, claims map[string]any
	_ = json.Unmarshal(must(t, parts[0]), &head)
	_ = json.Unmarshal(must(t, parts[1]), &claims)
	if head["alg"] != "ES256" || head["typ"] != "JWT" || claims["aud"] != "https://push.example.net:8443" ||
		claims["sub"] != "mailto:ops@example.org" || claims["exp"] != float64(now.Add(12*time.Hour).Unix()) {
		t.Fatalf("head %v claims %v", head, claims)
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), must(t, k))
	if err != nil {
		t.Fatal(err)
	}
	sig := must(t, parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("signature does not verify")
	}
	if _, err := ParseVAPID([]byte("short"), ""); err == nil {
		t.Fatal("a bad key parsed")
	}
}

// 005-AC24: Send posts the encrypted body with its headers to a test push endpoint, which the user
// agent's key decrypts; 404, 410, a 403 and a 401 for another VAPID key are ErrGone, any other 401
// and a 403 blaming the JWT ErrUnauthorized, any other refusal an error.
func TestSend(t *testing.T) {
	uaPriv, _ := ecdh.P256().NewPrivateKey(must(t, rfcUAPrivate))
	raw, _ := NewVAPIDKey()
	v, _ := ParseVAPID(raw, "mailto:ops@example.org")
	status, reply := http.StatusCreated, ""
	var got *http.Request
	var body []byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	defer srv.Close()
	sub := rfcSub()
	sub.Endpoint = srv.URL + "/push/abc"
	m := Message{Payload: []byte(`{"room":"Pricing"}`), TTL: time.Hour, Topic: "room1", Urgency: "normal"}
	if err := v.Send(t.Context(), srv.Client(), sub, m, time.Now()); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"Content-Encoding": "aes128gcm", "TTL": "3600", "Topic": "room1", "Urgency": "normal"} {
		if g := got.Header.Get(k); g != want {
			t.Errorf("%s: %q, want %q", k, g, want)
		}
	}
	if !strings.HasPrefix(got.Header.Get("Authorization"), "vapid t=") {
		t.Errorf("authorization %q", got.Header.Get("Authorization"))
	}
	if p := decrypt(t, uaPriv, must(t, rfcAuth), body); string(p) != `{"room":"Pricing"}` {
		t.Fatalf("payload %q", p)
	}
	for _, c := range []struct {
		code int
		body string
		want error
	}{
		{http.StatusGone, "", ErrGone},
		{http.StatusNotFound, "", ErrGone},
		{http.StatusForbidden, "", ErrGone},
		{http.StatusForbidden, "the VAPID credentials in the authorization header do not correspond to the credentials used to create the subscriptions.", ErrGone},
		{http.StatusForbidden, `{"reason":"BadJwtToken"}`, ErrUnauthorized},
		{http.StatusForbidden, "bad jwt", ErrUnauthorized},
		{http.StatusUnauthorized, `{"code": 401, "errno": 109, "error": "Unauthorized", "message": "VAPID public key mismatch"}`, ErrGone},
		{http.StatusUnauthorized, `{"errno":109}`, ErrGone},
		{http.StatusUnauthorized, "vapid public KEY mismatch", ErrGone},
		{http.StatusUnauthorized, "the VAPID credentials in the authorization header do not correspond to the credentials used to create the subscriptions.", ErrGone},
		{http.StatusUnauthorized, `{"errno": 1090}`, ErrUnauthorized},
		{http.StatusUnauthorized, `{"errno": 108, "message": "Invalid JWT"}`, ErrUnauthorized},
		{http.StatusUnauthorized, "", ErrUnauthorized},
	} {
		status, reply = c.code, c.body
		err := v.Send(t.Context(), srv.Client(), sub, m, time.Now())
		if !errors.Is(err, c.want) || errors.Is(err, ErrGone) && c.want != ErrGone {
			t.Errorf("%d %s: %v, want %v", c.code, c.body, err, c.want)
		}
	}
	status, reply = http.StatusTooManyRequests, ""
	if err := v.Send(t.Context(), srv.Client(), sub, m, time.Now()); err == nil || errors.Is(err, ErrGone) || !strings.Contains(err.Error(), "429") {
		t.Errorf("429: %v", err)
	}
}
