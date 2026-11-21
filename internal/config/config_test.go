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

type fakeSpeech struct {
	APIKey string `yaml:"api_key"`
	Voice  string `yaml:"voice"`
	Format string `yaml:"format"`
}

func (o *fakeSpeech) Validate() error {
	if o.Voice == "" {
		return errors.New("voice is required")
	}
	return nil
}

var providers = ProviderRegistry{"speech": {NewOptions: func() any { return &fakeSpeech{Format: "mp3"} }}}

type fakeAgent struct {
	Command []string `yaml:"command"`
}

func (o *fakeAgent) Validate() error {
	if len(o.Command) == 0 {
		return errors.New("command is required")
	}
	return nil
}

var backends = BackendRegistry{
	"pi":  {NewOptions: func() any { return &fakeAgent{Command: []string{"pi"}} }},
	"acp": {NewOptions: func() any { return &fakeAgent{} }},
}
var regs = Registries{Frontends: registry, Backends: backends, Providers: providers}

const base = "frontends:\n  main:\n    type: chat\n    owner: \"1\"\n    options:\n      token: yaml-token\nroutes:\n  - from: \"main:*\"\n    to: default\napi:\n  token: injection\nmcp:\n  token: files\n"

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
	t.Setenv("ROUTER_BACKENDS__DEFAULT__OPTIONS__COMMAND", `["fake", "--rpc"]`)
	c, err := Load(path, dotenv, regs)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Frontends["main"].Options.(*fakeOptions)
	if o.Token != "process-token" || o.Style != "auto" || !c.Debug || c.Media.TTL != 12*time.Hour || c.Stream.Separators[0] != "END" || c.Backends["default"].Options.(*fakeAgent).Command[0] != "fake" {
		t.Fatalf("incorrect layering: %+v %+v", c, o)
	}
	t.Setenv("ROUTER_STREAM__TYPO", "true")
	if _, err = Load(path, dotenv, regs); err == nil {
		t.Fatal("unknown env field accepted")
	}
}

func TestFrontendFromEnvironmentOnly(t *testing.T) {
	path, dotenv := write(t, "api:\n  token: a\nmcp:\n  token: b\n")
	t.Setenv("ROUTER_FRONTENDS__ENV__TYPE", "chat")
	t.Setenv("ROUTER_FRONTENDS__ENV__OWNER", "42")
	t.Setenv("ROUTER_FRONTENDS__ENV__OPTIONS__TOKEN", "t")
	t.Setenv("ROUTER_ROUTES", `[{"from": "env:*", "to": "default"}]`)
	t.Setenv("ROUTER_LIFECYCLE_NOTIFY", `["env"]`)
	c, err := Load(path, dotenv, regs)
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
		_, err := Load(path, dotenv, regs)
		if err == nil {
			t.Errorf("%s accepted", name)
		}
		t.Logf("%s: %v", name, err)
	}
	ok := strings.Replace(base, "type: chat", "type: chat\n    ingress:\n      path: /hooks/main", 1) + "ingress:\n  listen: 0.0.0.0:8443\nlifecycle_notify: [main]\n"
	path, dotenv := write(t, ok)
	if _, err := Load(path, dotenv, regs); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"hooks", "/hooks/", "/{x}", "/a b"} {
		path, dotenv = write(t, strings.Replace(ok, "/hooks/main", bad, 1))
		if _, err := Load(path, dotenv, regs); err == nil {
			t.Errorf("ingress path %q accepted", bad)
		}
	}
	path, dotenv = write(t, strings.Replace(ok, "0.0.0.0:8443", "127.0.0.1:8080", 1))
	if _, err := Load(path, dotenv, regs); err == nil {
		t.Error("ingress sharing the API listener accepted")
	}
}

func TestValidation(t *testing.T) {
	path, dotenv := write(t, base)
	c, err := Load(path, dotenv, regs)
	if err != nil {
		t.Fatal(err)
	}
	c.MCP.Listen = "0.0.0.0:8081"
	if c.Validate(regs) == nil {
		t.Fatal("public MCP accepted")
	}
	c.MCP.Listen = "127.0.0.1:8081"
	c.Stream.Separators = []string{""}
	if c.Validate(regs) == nil {
		t.Fatal("empty delimiter accepted")
	}
}

func TestTimestampConfiguration(t *testing.T) {
	path, dotenv := write(t, base+"timestamp:\n  enabled: true\n  timezone: Europe/London\n  format: '[15:04] '\n")
	c, err := Load(path, dotenv, regs)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Timestamp.Enabled || c.Timestamp.Timezone != "Europe/London" || c.Timestamp.Format != "[15:04] " || c.Timestamp.Location == nil || c.Timestamp.Location.String() != "Europe/London" {
		t.Fatalf("incorrect config: %+v", c.Timestamp)
	}
	t.Setenv("ROUTER_TIMESTAMP__TIMEZONE", "Asia/Shanghai")
	t.Setenv("ROUTER_TIMESTAMP__FORMAT", "15:04\n")
	c, err = Load(path, dotenv, regs)
	if err != nil || c.Timestamp.Timezone != "Asia/Shanghai" || c.Timestamp.Format != "15:04\n" || c.Timestamp.Location.String() != "Asia/Shanghai" {
		t.Fatalf("override failed: %+v %v", c.Timestamp, err)
	}
	for _, zone := range []string{"", "Invalid/Zone"} {
		c.Timestamp.Timezone = zone
		if c.Validate(regs) == nil {
			t.Fatalf("accepted timezone %q", zone)
		}
	}
	c.Timestamp.Timezone = "UTC"
	c.Timestamp.Format = ""
	if c.Validate(regs) == nil {
		t.Fatal("accepted empty format")
	}
	c.Timestamp.Enabled = false
	if err := c.Validate(regs); err != nil {
		t.Fatal(err)
	}
	defaults := Defaults()
	if defaults.Timestamp.Enabled || defaults.Timestamp.Timezone != "UTC" || defaults.Timestamp.Format == "" {
		t.Fatal("incorrect defaults")
	}
}

func TestSendVoiceConfiguration(t *testing.T) {
	// Disabled by default, even though the default provider type is not
	// registered here.
	path, dotenv := write(t, base)
	c, err := Load(path, dotenv, regs)
	if err != nil || c.MCP.Tools.SendVoice.Enabled {
		t.Fatal(c.MCP.Tools.SendVoice, err)
	}
	if Defaults().MCP.Tools.SendVoice.Provider.Type != "openai_speech" {
		t.Fatal("default provider type")
	}
	voice := base + "  tools:\n    send_voice:\n      enabled: true\n      style:\n        enabled: true\n        base: calm\n      provider:\n        type: speech\n        options:\n          voice: nova\n"
	path, dotenv = write(t, voice)
	t.Setenv("ROUTER_MCP__TOOLS__SEND_VOICE__PROVIDER__OPTIONS__API_KEY", "secret")
	c, err = Load(path, dotenv, regs)
	if err != nil {
		t.Fatal(err)
	}
	v := c.MCP.Tools.SendVoice
	o := v.Provider.Options.(*fakeSpeech)
	if !v.Enabled || !v.Style.Enabled || v.Style.Base != "calm" || o.Voice != "nova" || o.APIKey != "secret" || o.Format != "mp3" || c.MCP.Listen != "127.0.0.1:8081" {
		t.Fatalf("incorrect send_voice config: %+v %+v", v, o)
	}
	cases := map[string]string{
		"unknown option":   strings.Replace(voice, "voice: nova", "voice: nova\n          typo: 1", 1),
		"unknown type":     strings.Replace(voice, "type: speech", "type: fax", 1),
		"adapter validate": strings.Replace(voice, "voice: nova", "voice: \"\"", 1),
		"unknown field":    strings.Replace(voice, "enabled: true\n      style", "enabled: true\n      typo: 1\n      style", 1),
	}
	for name, body := range cases {
		path, dotenv := write(t, body)
		if _, err := Load(path, dotenv, regs); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestSendVoiceFromEnvironmentOnly(t *testing.T) {
	path, dotenv := write(t, base)
	t.Setenv("ROUTER_MCP__TOOLS__SEND_VOICE__ENABLED", "true")
	t.Setenv("ROUTER_MCP__TOOLS__SEND_VOICE__PROVIDER__TYPE", "speech")
	t.Setenv("ROUTER_MCP__TOOLS__SEND_VOICE__PROVIDER__OPTIONS__VOICE", "alloy")
	c, err := Load(path, dotenv, regs)
	if err != nil {
		t.Fatal(err)
	}
	if c.MCP.Tools.SendVoice.Provider.Options.(*fakeSpeech).Voice != "alloy" {
		t.Fatalf("env-only provider not built: %+v", c.MCP.Tools.SendVoice)
	}
	t.Setenv("ROUTER_MCP__TOOLS__SEND_VOICE__PROVIDER__TYPE", "openai_speech")
	if _, err = Load(path, dotenv, regs); err == nil {
		t.Fatal("enabled tool with unregistered provider accepted")
	}
}

func TestShowToolCalls(t *testing.T) {
	for body, want := range map[string]ToolCalls{
		"":                                   {Enabled: false, Detailed: true, MaxDetailLength: 120},
		"stream:\n  show_tool_calls: true\n": {Enabled: true, Detailed: true, MaxDetailLength: 120},
		"stream:\n  show_tool_calls:\n    enabled: true\n    detailed: false\n    max_detail_length: 40\n": {Enabled: true, Detailed: false, MaxDetailLength: 40},
	} {
		path, dotenv := write(t, body+base)
		c, err := Load(path, dotenv, regs)
		if err != nil || c.Stream.ShowToolCalls != want {
			t.Fatalf("%q: %+v %v", body, c.Stream.ShowToolCalls, err)
		}
	}
	t.Setenv("ROUTER_STREAM__SHOW_TOOL_CALLS__ENABLED", "true")
	path, dotenv := write(t, base)
	if c, err := Load(path, dotenv, regs); err != nil || !c.Stream.ShowToolCalls.Enabled {
		t.Fatalf("environment override: %+v %v", c.Stream.ShowToolCalls, err)
	}
	for _, body := range []string{"stream:\n  show_tool_calls:\n    enabeld: true\n", "stream:\n  show_tool_calls:\n    max_detail_length: 0\n"} {
		path, dotenv := write(t, body+base)
		if _, err := Load(path, dotenv, regs); err == nil {
			t.Fatalf("%q: expected an error", body)
		}
	}
}

func TestBackendInstances(t *testing.T) {
	routes := "routes:\n  - from: \"main:*\"\n    to: coder\n"
	front := strings.Split(base, "routes:")[0]
	tail := "api:\n  token: injection\nmcp:\n  token: files\n"
	path, dotenv := write(t, front+routes+tail+"backends:\n  coder:\n    type: acp\n    busy_policy: interrupt\n    options:\n      command: [opencode, acp]\n")
	c, err := Load(path, dotenv, regs)
	if err != nil {
		t.Fatal(err)
	}
	// Defining backends replaces the implicit default.
	b, ok := c.Backends["coder"]
	if len(c.Backends) != 1 || !ok || b.BusyPolicy != "interrupt" || b.Options.(*fakeAgent).Command[1] != "acp" {
		t.Fatalf("%+v", c.Backends)
	}
	for name, body := range map[string]string{
		"unknown type":   "backends:\n  coder:\n    type: nope\n",
		"bad policy":     "backends:\n  coder:\n    type: acp\n    busy_policy: drop\n    options:\n      command: [x]\n",
		"invalid opts":   "backends:\n  coder:\n    type: acp\n",
		"unknown option": "backends:\n  coder:\n    type: acp\n    options:\n      command: [x]\n      bogus: 1\n",
		"old top level":  "backends:\n  coder:\n    type: acp\n    command: [x]\n",
		"bad name":       "backends:\n  Coder:\n    type: acp\n    options:\n      command: [x]\n",
	} {
		t.Run(name, func(t *testing.T) {
			path, dotenv := write(t, front+routes+tail+body)
			if _, err := Load(path, dotenv, regs); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	typed, typedEnv := write(t, front+"routes:\n  - from: \"main:*\"\n    to: pi:default\n"+tail)
	if _, err = Load(typed, typedEnv, regs); err == nil {
		t.Fatal("typed route destination accepted")
	}
	t.Setenv("ROUTER_BACKENDS__CODER__OPTIONS__COMMAND", `["other"]`)
	if c, err = Load(path, dotenv, regs); err != nil || c.Backends["coder"].Options.(*fakeAgent).Command[0] != "other" {
		t.Fatalf("env override: %v", err)
	}
}

func TestStateDirIsRelativeToConfig(t *testing.T) {
	path, dotenv := write(t, base)
	c, err := Load(path, dotenv, regs)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(path), "state"); c.State.Dir != want {
		t.Fatalf("state dir %s, want %s", c.State.Dir, want)
	}
	path, dotenv = write(t, base+"state:\n  dir: /srv/state\n")
	if c, err = Load(path, dotenv, regs); err != nil || c.State.Dir != "/srv/state" {
		t.Fatalf("absolute dir changed: %s %v", c.State.Dir, err)
	}
}
