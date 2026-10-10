// Package livekit mints LiveKit access tokens and calls the two RoomService methods the room
// server needs, with the standard library only.
package livekit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Grant is the "video" claim of a LiveKit token.
type Grant struct {
	Room              string   `json:"room,omitempty"`
	RoomJoin          bool     `json:"roomJoin,omitempty"`
	RoomAdmin         bool     `json:"roomAdmin,omitempty"`
	RoomCreate        bool     `json:"roomCreate,omitempty"`
	CanPublish        *bool    `json:"canPublish,omitempty"`
	CanSubscribe      *bool    `json:"canSubscribe,omitempty"`
	CanPublishData    *bool    `json:"canPublishData,omitempty"`
	CanPublishSources []string `json:"canPublishSources,omitempty"`
}

// MinSecretLen is LiveKit's own floor for an API secret.
const MinSecretLen = 32

// Token returns an HS256 JWT signed with secret, valid from now for ttl.
func Token(key, secret, identity, name string, g Grant, ttl time.Duration, now time.Time) (string, error) {
	claims := map[string]any{
		"iss":   key,
		"sub":   identity,
		"nbf":   now.Add(-10 * time.Second).Unix(), // tolerate clock skew with the LiveKit host
		"exp":   now.Add(ttl).Unix(),
		"video": g,
	}
	if name != "" {
		claims["name"] = name
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("livekit token: %w", err)
	}
	enc := base64.RawURLEncoding
	unsigned := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(unsigned))
	return unsigned + "." + enc.EncodeToString(mac.Sum(nil)), nil
}

// Client calls LiveKit's RoomService over its Twirp JSON API.
type Client struct {
	// URL is LiveKit's HTTP base, as the room server reaches it (http://livekit:7880 in compose).
	URL         string
	Key, Secret string
	HTTP        *http.Client
}

// RemoveParticipant disconnects identity from room. An absent room or participant is not an error.
func (c *Client) RemoveParticipant(ctx context.Context, room, identity string) error {
	return c.call(ctx, "RemoveParticipant", Grant{Room: room, RoomAdmin: true}, map[string]string{"room": room, "identity": identity})
}

// DeleteRoom closes room and disconnects everyone in it. An absent room is not an error.
func (c *Client) DeleteRoom(ctx context.Context, room string) error {
	return c.call(ctx, "DeleteRoom", Grant{Room: room, RoomCreate: true}, map[string]string{"room": room})
}

func (c *Client) call(ctx context.Context, method string, g Grant, req any) error {
	tok, err := Token(c.Key, c.Secret, "grill-room", "", g, time.Minute, time.Now())
	if err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("livekit %s: %w", method, err)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+"/twirp/livekit.RoomService/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("livekit %s: %w", method, err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return fmt.Errorf("livekit %s: %w", method, err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("livekit %s: HTTP %d: %s", method, resp.StatusCode, bytes.TrimSpace(msg))
}
