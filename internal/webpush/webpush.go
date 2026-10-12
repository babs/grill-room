// Package webpush sends Web Push messages: payloads encrypted as RFC 8291 says (aes128gcm, RFC
// 8188), authenticated with a VAPID JWT (RFC 8292). Standard library only.
package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ErrGone is returned when the push service says the subscription no longer exists (404, 410):
// the caller drops it.
var ErrGone = errors.New("push subscription gone")

// recordSize is the aes128gcm record size the header announces. One record holds the whole
// payload, so it only needs to exceed it: 4096 is what push services accept (RFC 8291 section 4).
const recordSize = 4096

// MaxPayload is the largest plaintext one record holds: the record size less the padding
// delimiter and the 16-octet GCM tag.
const MaxPayload = recordSize - 1 - 16

var b64 = base64.RawURLEncoding

// Subscription is what the browser's PushSubscription.toJSON() gives: the push service endpoint
// and the user agent's P-256 public key and auth secret, base64url.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// Check decodes the keys and the endpoint, as a malformed subscription is refused when stored,
// not when a push fails.
func (s *Subscription) Check() error {
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || len(s.Endpoint) > 2048 {
		return errors.New("endpoint must be an https URL of at most 2048 characters")
	}
	if _, err := s.keys(); err != nil {
		return err
	}
	return nil
}

type uaKeys struct {
	public *ecdh.PublicKey
	auth   []byte
}

func (s *Subscription) keys() (uaKeys, error) {
	raw, err := b64.DecodeString(s.Keys.P256dh)
	if err != nil {
		return uaKeys{}, errors.New("keys.p256dh is not base64url")
	}
	// NewPublicKey checks the point is on the curve (RFC 8291 section 7).
	pub, err := ecdh.P256().NewPublicKey(raw)
	if err != nil {
		return uaKeys{}, errors.New("keys.p256dh is not an uncompressed P-256 point")
	}
	auth, err := b64.DecodeString(s.Keys.Auth)
	if err != nil || len(auth) != 16 {
		return uaKeys{}, errors.New("keys.auth must be 16 octets, base64url")
	}
	return uaKeys{pub, auth}, nil
}

// encrypt is RFC 8291 section 3.4 with the sender's key pair and salt given: the test vector
// fixes them, a push draws them at random.
func encrypt(ua uaKeys, plaintext []byte, as *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(plaintext) > MaxPayload {
		return nil, fmt.Errorf("payload of %d octets, at most %d", len(plaintext), MaxPayload)
	}
	secret, err := as.ECDH(ua.public)
	if err != nil {
		return nil, fmt.Errorf("ecdh: %w", err)
	}
	asPublic := as.PublicKey().Bytes()
	keyInfo := "WebPush: info\x00" + string(ua.public.Bytes()) + string(asPublic)
	ikm, err := hkdf.Key(sha256.New, secret, ua.auth, keyInfo, 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// Header (RFC 8188 section 2.1): salt, record size, key id length and key id.
	out := make([]byte, 0, 16+4+1+len(asPublic)+len(plaintext)+1+gcm.Overhead())
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, recordSize)
	out = append(out, 65) // the key id length: an uncompressed P-256 point
	out = append(out, asPublic...)
	// The last and only record ends with the 0x02 delimiter.
	return gcm.Seal(out, nonce, append(append([]byte{}, plaintext...), 2), nil), nil
}

// Encrypt encrypts plaintext for sub with a fresh sender key pair and salt.
func Encrypt(sub *Subscription, plaintext []byte) ([]byte, error) {
	ua, err := sub.keys()
	if err != nil {
		return nil, err
	}
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return encrypt(ua, plaintext, as, salt)
}

// Decrypt is the user agent's side of RFC 8291, for tests and tools that stand in for a browser:
// the plaintext of body for the key pair uaPriv and its auth secret.
func Decrypt(uaPriv *ecdh.PrivateKey, auth, body []byte) ([]byte, error) {
	if len(body) < 21 || len(body) < 21+int(body[20]) {
		return nil, errors.New("body shorter than its header")
	}
	salt, idlen := body[:16], int(body[20])
	asPub, err := ecdh.P256().NewPublicKey(body[21 : 21+idlen])
	if err != nil {
		return nil, fmt.Errorf("key id: %w", err)
	}
	secret, err := uaPriv.ECDH(asPub)
	if err != nil {
		return nil, err
	}
	ikm, err := hkdf.Key(sha256.New, secret, auth, "WebPush: info\x00"+string(uaPriv.PublicKey().Bytes())+string(asPub.Bytes()), 32)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		return nil, err
	}
	if len(plain) == 0 || plain[len(plain)-1] != 2 {
		return nil, errors.New("not a last record")
	}
	return plain[:len(plain)-1], nil
}

// VAPID is the application server's identity toward push services (RFC 8292): a P-256 key
// pair, and a contact in the JWT's sub claim.
type VAPID struct {
	key     *ecdsa.PrivateKey
	subject string
}

// NewVAPIDKey generates a private key, as the raw 32 octets ParseVAPID takes.
func NewVAPIDKey() ([]byte, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return k.Bytes()
}

// ParseVAPID loads a private key NewVAPIDKey made. subject is a mailto: or https: contact.
func ParseVAPID(raw []byte, subject string) (*VAPID, error) {
	k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
	if err != nil {
		return nil, fmt.Errorf("vapid key: %w", err)
	}
	return &VAPID{key: k, subject: subject}, nil
}

// PublicKey is the key the browser's pushManager.subscribe takes as applicationServerKey:
// uncompressed, base64url.
func (v *VAPID) PublicKey() string {
	b, _ := v.key.PublicKey.Bytes() // a key ParseRawPrivateKey accepted always encodes
	return b64.EncodeToString(b)
}

// Authorization is the header value for a push to endpoint: a JWT for the endpoint's origin,
// valid 12 hours (RFC 8292 caps it at 24), and the public key.
func (v *VAPID) Authorization(endpoint string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{"aud": u.Scheme + "://" + u.Host, "exp": now.Add(12 * time.Hour).Unix(), "sub": v.subject})
	if err != nil {
		return "", err
	}
	signing := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`)) + "." + b64.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, v.key, digest[:])
	if err != nil {
		return "", err
	}
	// JWS wants r and s as two 32-octet big-endian integers, not ASN.1 (RFC 7518 section 3.4).
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signing + "." + b64.EncodeToString(sig) + ", k=" + v.PublicKey(), nil
}

// Message is one push: its payload, how long the push service may hold it, and a topic that
// replaces an undelivered message with the same one (RFC 8030 section 5.4).
type Message struct {
	Payload []byte
	TTL     time.Duration
	Topic   string
	Urgency string
}

// Send encrypts m for sub and posts it to the push service. ErrGone when the subscription no
// longer exists; any other refusal is an error with the status.
func (v *VAPID) Send(ctx context.Context, c *http.Client, sub *Subscription, m Message, now time.Time) error {
	body, err := Encrypt(sub, m.Payload)
	if err != nil {
		return err
	}
	auth, err := v.Authorization(sub.Endpoint, now)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(int(m.TTL/time.Second)))
	if m.Topic != "" {
		req.Header.Set("Topic", m.Topic)
	}
	if m.Urgency != "" {
		req.Header.Set("Urgency", m.Urgency)
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	defer resp.Body.Close()
	reason, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrGone
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("push: %s: %s", resp.Status, bytes.TrimSpace(reason))
	}
	return nil
}
