package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLayersAndTypedOverrides(t *testing.T) {
	dir := t.TempDir()
	yaml := filepath.Join(dir, "config.yaml")
	dotenv := filepath.Join(dir, ".env")
	_ = os.WriteFile(yaml, []byte("debug: false\nmedia:\n  ttl: 12h\ntelegram:\n  token: yaml-token\n  user_id: 1\napi:\n  token: injection\nmcp:\n  token: files\n"), 0600)
	_ = os.WriteFile(dotenv, []byte("ROUTER_TELEGRAM__TOKEN='dotenv-token'\nROUTER_DEBUG=true\n"), 0600)
	t.Setenv("ROUTER_TELEGRAM__TOKEN", "process-token")
	t.Setenv("ROUTER_STREAM__SEPARATORS", `["END"]`)
	t.Setenv("ROUTER_BACKENDS__DEFAULT__COMMAND", `["fake", "--rpc"]`)
	c, err := Load(yaml, dotenv)
	if err != nil {
		t.Fatal(err)
	}
	if c.Telegram.Token != "process-token" || !c.Debug || c.Media.TTL != 12*time.Hour || c.Stream.Separators[0] != "END" || c.Backends["default"].Command[0] != "fake" {
		t.Fatalf("incorrect layering: %+v", c)
	}
	t.Setenv("ROUTER_STREAM__TYPO", "true")
	if _, err = Load(yaml, dotenv); err == nil {
		t.Fatal("unknown env field accepted")
	}
}
func TestValidation(t *testing.T) {
	c := Defaults()
	c.Telegram.Token = "x"
	c.Telegram.UserID = 1
	c.API.Token = "a"
	c.MCP.Token = "b"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.MCP.Listen = "0.0.0.0:8081"
	if c.Validate() == nil {
		t.Fatal("public MCP accepted")
	}
	c.MCP.Listen = "127.0.0.1:8081"
	c.Stream.Separators = []string{""}
	if c.Validate() == nil {
		t.Fatal("empty delimiter accepted")
	}
}

func TestTimestampConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("telegram:\n  token: test\n  user_id: 1\napi:\n  token: a\nmcp:\n  token: b\ntimestamp:\n  enabled: true\n  timezone: Europe/London\n  format: '[15:04] '\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Timestamp.Enabled || c.Timestamp.Timezone != "Europe/London" || c.Timestamp.Format != "[15:04] " || c.Timestamp.Location == nil || c.Timestamp.Location.String() != "Europe/London" {
		t.Fatalf("incorrect config: %+v", c.Timestamp)
	}
	t.Setenv("ROUTER_TIMESTAMP__TIMEZONE", "Asia/Shanghai")
	t.Setenv("ROUTER_TIMESTAMP__FORMAT", "15:04\n")
	c, err = Load(path, filepath.Join(dir, ".env"))
	if err != nil || c.Timestamp.Timezone != "Asia/Shanghai" || c.Timestamp.Format != "15:04\n" || c.Timestamp.Location.String() != "Asia/Shanghai" {
		t.Fatalf("override failed: %+v %v", c.Timestamp, err)
	}
	for _, zone := range []string{"", "Invalid/Zone"} {
		c.Timestamp.Timezone = zone
		if c.Validate() == nil {
			t.Fatalf("accepted timezone %q", zone)
		}
	}
	c.Timestamp.Timezone = "UTC"
	c.Timestamp.Format = ""
	if c.Validate() == nil {
		t.Fatal("accepted empty format")
	}
	c.Timestamp.Enabled = false
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	defaults := Defaults()
	if defaults.Timestamp.Enabled || defaults.Timestamp.Timezone != "UTC" || defaults.Timestamp.Format == "" {
		t.Fatal("incorrect defaults")
	}
}
