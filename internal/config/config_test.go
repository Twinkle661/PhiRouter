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
