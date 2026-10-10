package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/web"
)

func TestConfigValidate(t *testing.T) {
	ok := Config{Addr: "a:1", MetricsAddr: "a:2", DB: "x.db", LogLevel: "info", LogFormat: "auto",
		MaxUploadMB: 20, MaxRoomMB: 200, MaxAttachmentMB: 5, Pandoc: "pandoc", PandocTimeout: time.Minute}
	cases := map[string]func(*Config){
		"zero upload limit":       func(c *Config) { c.MaxUploadMB = 0 },
		"room below upload limit": func(c *Config) { c.MaxRoomMB = 10 },
		"room limit overflows":    func(c *Config) { c.MaxRoomMB = 1 << 21 },
		"zero attachment limit":   func(c *Config) { c.MaxAttachmentMB = 0 },
		"attachment over room":    func(c *Config) { c.MaxAttachmentMB = 201 },
		"zero pandoc timeout":     func(c *Config) { c.PandocTimeout = 0 },
		"same addr":               func(c *Config) { c.MetricsAddr = c.Addr },
		"empty db":                func(c *Config) { c.DB = "" },
		"bad level":               func(c *Config) { c.LogLevel = "loud" },
		"bad format":              func(c *Config) { c.LogFormat = "xml" },
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	for name, mutate := range cases {
		c := ok
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadConfigRejectsBadNumbers(t *testing.T) {
	for key, val := range map[string]string{"GRILL_ROOM_MAX_UPLOAD_MB": "20MB", "GRILL_ROOM_MAX_ROOM_MB": "x", "GRILL_ROOM_PANDOC_TIMEOUT": "60", "GRILL_ROOM_MAX_ATTACHMENT_MB": "5MB"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, val)
			if _, err := loadConfig(); err == nil {
				t.Fatalf("%s=%s accepted", key, val)
			}
		})
	}
	c, err := loadConfig()
	if err != nil || c.MaxUploadMB != 20 || c.MaxRoomMB != 200 || c.MaxAttachmentMB != 5 || c.Validate() != nil {
		t.Fatalf("defaults: %+v, %v", c, err)
	}
}

func TestAdminTokenHash(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()

	hash, generated, err := adminTokenHash(ctx, st, "")
	if err != nil || generated == "" || hash != web.HashToken(generated) {
		t.Fatalf("first start: %q %q %v", hash, generated, err)
	}
	stored, _ := st.Setting(ctx, adminTokenSetting)
	if stored != hash {
		t.Fatal("hash not persisted")
	}
	// Second start: same hash, nothing to show.
	hash2, generated2, err := adminTokenHash(ctx, st, "")
	if err != nil || hash2 != hash || generated2 != "" {
		t.Fatalf("restart: %q %q %v", hash2, generated2, err)
	}
	// The environment wins and does not touch the stored hash.
	hash3, generated3, err := adminTokenHash(ctx, st, "pinned")
	if err != nil || hash3 != web.HashToken("pinned") || generated3 != "" {
		t.Fatalf("env: %q %q %v", hash3, generated3, err)
	}
	if stored, _ := st.Setting(ctx, adminTokenSetting); stored != hash {
		t.Fatal("env token overwrote the stored hash")
	}
}

func TestValidateVoice(t *testing.T) {
	ok := Config{LiveKitURL: "wss://livekit.example", LiveKitAPIURL: "http://livekit:7880", LiveKitAPIKey: "k",
		LiveKitAPISecret: "0123456789abcdef0123456789abcdef", STTToken: "s"}
	if err := ok.validateVoice(); err != nil {
		t.Fatalf("valid: %v", err)
	}
	if err := (Config{}).validateVoice(); err != nil {
		t.Fatalf("voice off: %v", err)
	}
	cases := map[string]func(*Config){
		"stt without livekit": func(c *Config) { *c = Config{STTToken: "s"} },
		"http signalling":     func(c *Config) { c.LiveKitURL = "https://livekit.example" },
		"no host":             func(c *Config) { c.LiveKitURL = "wss://" },
		"ws api url":          func(c *Config) { c.LiveKitAPIURL = "ws://livekit:7880" },
		"no key":              func(c *Config) { c.LiveKitAPIKey = "" },
		"short secret":        func(c *Config) { c.LiveKitAPISecret = "short" },
	}
	for name, mutate := range cases {
		c := ok
		mutate(&c)
		if err := c.validateVoice(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	v := ok.voice()
	if v == nil || v.STTTokenHash != web.HashToken("s") || v.API.URL != "http://livekit:7880" {
		t.Fatalf("voice %+v", v)
	}
	if (Config{}).voice() != nil {
		t.Fatal("voice on without a URL")
	}
	t.Setenv("GRILL_ROOM_LIVEKIT_URL", "wss://livekit.example")
	if c, _ := loadConfig(); c.LiveKitAPIURL != "https://livekit.example" {
		t.Fatalf("default API URL %q", c.LiveKitAPIURL)
	}
}
