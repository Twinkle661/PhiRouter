package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeOptions struct {
	Token string `yaml:"token"`
	Style string `yaml:"style"`
}

func (o *fakeOptions) Validate() error {
	if o.Token == "" {
		return errors.New("token is required")
	}
	return nil
}

var registry = Registry{"chat": {
	NewOptions: func() any { return &fakeOptions{Style: "auto"} },
	ValidAddress: func(s string) error {
		if strings.ContainsAny(s, " *") {
			return errors.New("bad address")
		}
		return nil
	},
}}

const base = "frontends:\n  main:\n    type: chat\n    owner: \"1\"\n    options:\n      token: yaml-token\nroutes:\n  - from: \"main:*\"\n    to: pi:default\napi:\n  token: injection\nmcp:\n  token: files\n"

func write(t *testing.T, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path, filepath.Join(dir, ".env")
}

func TestLayersAndTypedOverrides(t *testing.T) {
	path, dotenv := write(t, "debug: false\nmedia:\n  ttl: 12h\n"+base)
	_ = os.WriteFile(dotenv, []byte("ROUTER_FRONTENDS__MAIN__OPTIONS__TOKEN='dotenv-token'\nROUTER_DEBUG=true\n"), 0600)
	t.Setenv("ROUTER_FRONTENDS__MAIN__OPTIONS__TOKEN", "process-token")
	t.Setenv("ROUTER_STREAM__SEPARATORS", `["END"]`)
	t.Setenv("ROUTER_BACKENDS__DEFAULT__COMMAND", `["fake", "--rpc"]`)
	c, err := Load(path, dotenv, registry)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Frontends["main"].Options.(*fakeOptions)
	if o.Token != "process-token" || o.Style != "auto" || !c.Debug || c.Media.TTL != 12*time.Hour || c.Stream.Separators[0] != "END" || c.Backends["default"].Command[0] != "fake" {
		t.Fatalf("incorrect layering: %+v %+v", c, o)
	}
	t.Setenv("ROUTER_STREAM__TYPO", "true")
	if _, err = Load(path, dotenv, registry); err == nil {
		t.Fatal("unknown env field accepted")
	}
}

func TestFrontendFromEnvironmentOnly(t *testing.T) {
	path, dotenv := write(t, "api:\n  token: a\nmcp:\n  token: b\n")
	t.Setenv("ROUTER_FRONTENDS__ENV__TYPE", "chat")
	t.Setenv("ROUTER_FRONTENDS__ENV__OWNER", "42")
	t.Setenv("ROUTER_FRONTENDS__ENV__OPTIONS__TOKEN", "t")
	t.Setenv("ROUTER_ROUTES", `[{"from": "env:*", "to": "pi:default"}]`)
	t.Setenv("ROUTER_LIFECYCLE_NOTIFY", `["env"]`)
	c, err := Load(path, dotenv, registry)
	if err != nil {
		t.Fatal(err)
	}
	if c.Frontends["env"].Options.(*fakeOptions).Token != "t" || c.LifecycleNotify[0] != "env" {
		t.Fatalf("env-only frontend not built: %+v", c.Frontends)
	}
}

func TestFrontendValidation(t *testing.T) {
	cases := map[string]string{
		"unknown option":    strings.Replace(base, "token: yaml-token", "token: x\n      typo: 1", 1),
		"unknown type":      strings.Replace(base, "type: chat", "type: fax", 1),
		"adapter validate":  strings.Replace(base, "token: yaml-token", "token: \"\"", 1),
		"missing owner":     strings.Replace(base, "owner: \"1\"", "owner: \"\"", 1),
		"bad owner":         strings.Replace(base, "owner: \"1\"", "owner: \"a b\"", 1),
		"bad route address": strings.Replace(base, "main:*", "main:a b", 1),
		"unknown frontend":  strings.Replace(base, "main:*", "other:*", 1),
		"bad instance name": strings.Replace(strings.Replace(base, "  main:", "  Main:", 1), "main:*", "Main:*", 1),
		"unknown notify":    base + "lifecycle_notify: [other]\n",
		"ingress no listen": strings.Replace(base, "type: chat", "type: chat\n    ingress:\n      path: /hooks/main", 1),
	}
	for name, body := range cases {
		path, dotenv := write(t, body)
		_, err := Load(path, dotenv, registry)
		if err == nil {
			t.Errorf("%s accepted", name)
		}
		t.Logf("%s: %v", name, err)
	}
	ok := strings.Replace(base, "type: chat", "type: chat\n    ingress:\n      path: /hooks/main", 1) + "ingress:\n  listen: 0.0.0.0:8443\nlifecycle_notify: [main]\n"
	path, dotenv := write(t, ok)
	if _, err := Load(path, dotenv, registry); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"hooks", "/hooks/", "/{x}", "/a b"} {
		path, dotenv = write(t, strings.Replace(ok, "/hooks/main", bad, 1))
		if _, err := Load(path, dotenv, registry); err == nil {
			t.Errorf("ingress path %q accepted", bad)
		}
	}
	path, dotenv = write(t, strings.Replace(ok, "0.0.0.0:8443", "127.0.0.1:8080", 1))
	if _, err := Load(path, dotenv, registry); err == nil {
		t.Error("ingress sharing the API listener accepted")
	}
}

func TestValidation(t *testing.T) {
	path, dotenv := write(t, base)
	c, err := Load(path, dotenv, registry)
	if err != nil {
		t.Fatal(err)
	}
	c.MCP.Listen = "0.0.0.0:8081"
	if c.Validate(registry) == nil {
		t.Fatal("public MCP accepted")
	}
	c.MCP.Listen = "127.0.0.1:8081"
	c.Stream.Separators = []string{""}
	if c.Validate(registry) == nil {
		t.Fatal("empty delimiter accepted")
	}
}

func TestTimestampConfiguration(t *testing.T) {
	path, dotenv := write(t, base+"timestamp:\n  enabled: true\n  timezone: Europe/London\n  format: '[15:04] '\n")
	c, err := Load(path, dotenv, registry)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Timestamp.Enabled || c.Timestamp.Timezone != "Europe/London" || c.Timestamp.Format != "[15:04] " || c.Timestamp.Location == nil || c.Timestamp.Location.String() != "Europe/London" {
		t.Fatalf("incorrect config: %+v", c.Timestamp)
	}
	t.Setenv("ROUTER_TIMESTAMP__TIMEZONE", "Asia/Shanghai")
	t.Setenv("ROUTER_TIMESTAMP__FORMAT", "15:04\n")
	c, err = Load(path, dotenv, registry)
	if err != nil || c.Timestamp.Timezone != "Asia/Shanghai" || c.Timestamp.Format != "15:04\n" || c.Timestamp.Location.String() != "Asia/Shanghai" {
		t.Fatalf("override failed: %+v %v", c.Timestamp, err)
	}
	for _, zone := range []string{"", "Invalid/Zone"} {
		c.Timestamp.Timezone = zone
		if c.Validate(registry) == nil {
			t.Fatalf("accepted timezone %q", zone)
		}
	}
	c.Timestamp.Timezone = "UTC"
	c.Timestamp.Format = ""
	if c.Validate(registry) == nil {
		t.Fatal("accepted empty format")
	}
	c.Timestamp.Enabled = false
	if err := c.Validate(registry); err != nil {
		t.Fatal(err)
	}
	defaults := Defaults()
	if defaults.Timestamp.Enabled || defaults.Timestamp.Timezone != "UTC" || defaults.Timestamp.Format == "" {
		t.Fatal("incorrect defaults")
	}
}
