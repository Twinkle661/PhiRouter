package config

import (
	"bufio"
	"bytes"
	"fmt"
	"gopkg.in/yaml.v3"
	"net"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Endpoint struct {
	Listen string `yaml:"listen"`
	Token  string `yaml:"token"`
}
type Config struct {
	Debug     bool                `yaml:"debug"`
	Frontends map[string]Frontend `yaml:"frontends"`
	Backends  map[string]Backend  `yaml:"backends"`
	Routes    []Route             `yaml:"routes"`
	Telegram  struct {
		Token  string `yaml:"token"`
		UserID int64  `yaml:"user_id"`
	} `yaml:"telegram"`
	Stream struct {
		Separators    []string `yaml:"separators"`
		ShowToolCalls bool     `yaml:"show_tool_calls"`
	} `yaml:"stream"`
	Media struct {
		Dir              string        `yaml:"dir"`
		InlineImageMaxMB int64         `yaml:"inline_image_max_mb"`
		TTL              time.Duration `yaml:"ttl"`
		VoiceMaxSeconds  float64       `yaml:"voice_max_seconds"`
	} `yaml:"media"`
	API     Endpoint `yaml:"api"`
	MCP     Endpoint `yaml:"mcp"`
	Prompts struct {
		Attachment string `yaml:"attachment"`
	} `yaml:"prompts"`
	Plugins    []Plugin `yaml:"plugins"`
	QueueLimit int      `yaml:"queue_limit"`
}
type Frontend struct {
	Type string `yaml:"type"`
}
type Backend struct {
	Type        string   `yaml:"type"`
	Command     []string `yaml:"command"`
	SessionFile string   `yaml:"session_file"`
}
type Route struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}
type Plugin struct {
	Type     string        `yaml:"type"`
	Prefix   string        `yaml:"prefix"`
	Interval time.Duration `yaml:"interval"`
	Route    string        `yaml:"route"`
	Text     string        `yaml:"text"`
}

func Defaults() Config {
	var c Config
	c.Frontends = map[string]Frontend{"telegram": {Type: "telegram"}}
	c.Backends = map[string]Backend{"default": {Type: "pi", Command: []string{"pi", "--mode", "rpc"}}}
	c.Routes = []Route{{"telegram:*", "pi:default"}}
	c.Stream.Separators = []string{"\n", "<break />"}
	c.Media.Dir = "/data/media"
	c.Media.InlineImageMaxMB = 5
	c.Media.TTL = 24 * time.Hour
	c.Media.VoiceMaxSeconds = 60
	c.API.Listen = "127.0.0.1:8080"
	c.MCP.Listen = "127.0.0.1:8081"
	c.Prompts.Attachment = "[attachment: {path} ({mime}, {size})]"
	c.QueueLimit = 256
	return c
}
func Load(path, dotenv string) (Config, error) {
	c := Defaults()
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return c, err
	}
	if err == nil {
		d := yaml.NewDecoder(bytes.NewReader(raw))
		d.KnownFields(true)
		if err = d.Decode(&c); err != nil {
			return c, fmt.Errorf("invalid YAML: %w", err)
		}
	}
	vars, err := readEnv(dotenv)
	if err != nil {
		return c, err
	}
	for _, s := range os.Environ() {
		k, v, _ := strings.Cut(s, "=")
		if strings.HasPrefix(k, "ROUTER_") {
			vars[k] = v
		}
	}
	// Apply typed overrides, including map entries and numeric slice indexes.
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := strings.Count(keys[i], "__"), strings.Count(keys[j], "__")
		if a != b {
			return a < b
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		v := vars[k]
		if !strings.HasPrefix(k, "ROUTER_") {
			continue
		}
		if err = set(reflect.ValueOf(&c).Elem(), strings.Split(strings.ToLower(strings.TrimPrefix(k, "ROUTER_")), "__"), v); err != nil {
			return c, fmt.Errorf("%s: %w", k, err)
		}
	}
	return c, c.Validate()
}
func readEnv(path string) (map[string]string, error) {
	out := map[string]string{}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 1024*1024)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("invalid .env assignment")
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == '"' {
			var e error
			v, e = strconv.Unquote(v)
			if e != nil {
				return nil, fmt.Errorf("invalid .env quoted value")
			}
		} else if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, s.Err()
}
func set(v reflect.Value, path []string, raw string) error {
	if len(path) == 0 {
		if v.Kind() == reflect.String {
			v.SetString(raw)
			return nil
		}
		p := reflect.New(v.Type())
		if err := yaml.Unmarshal([]byte(raw), p.Interface()); err != nil {
			return err
		}
		v.Set(p.Elem())
		return nil
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if strings.Split(v.Type().Field(i).Tag.Get("yaml"), ",")[0] == path[0] {
				return set(v.Field(i), path[1:], raw)
			}
		}
	case reflect.Map:
		key := reflect.ValueOf(path[0])
		old := v.MapIndex(key)
		n := reflect.New(v.Type().Elem()).Elem()
		if old.IsValid() {
			n.Set(old)
		}
		if err := set(n, path[1:], raw); err != nil {
			return err
		}
		if v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
		v.SetMapIndex(key, n)
		return nil
	case reflect.Slice:
		i, err := strconv.Atoi(path[0])
		if err == nil && i >= 0 && i < v.Len() {
			return set(v.Index(i), path[1:], raw)
		}
	}
	return fmt.Errorf("unknown configuration field %s", strings.Join(path, "."))
}
func (c Config) Validate() error {
	if c.Telegram.Token == "" || c.Telegram.UserID <= 0 {
		return fmt.Errorf("telegram.token and positive telegram.user_id are required")
	}
	if c.API.Token == "" || c.MCP.Token == "" || c.API.Token == c.MCP.Token {
		return fmt.Errorf("api.token and mcp.token must be nonempty and different")
	}
	for name, ep := range map[string]Endpoint{"api": c.API, "mcp": c.MCP} {
		h, p, err := net.SplitHostPort(ep.Listen)
		if err != nil {
			return fmt.Errorf("invalid %s.listen", name)
		}
		port, err := strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("invalid %s port", name)
		}
		ip := net.ParseIP(h)
		if name == "mcp" && h != "127.0.0.1" {
			return fmt.Errorf("mcp.listen must bind to 127.0.0.1")
		}
		if name == "api" && h != "" && h != "0.0.0.0" && (ip == nil || (!ip.IsPrivate() && !ip.IsLoopback())) {
			return fmt.Errorf("api.listen must use a private or loopback address")
		}
	}
	if c.API.Listen == c.MCP.Listen {
		return fmt.Errorf("API and MCP listeners must differ")
	}
	if c.Media.Dir == "" || c.Media.TTL <= 0 || c.Media.InlineImageMaxMB < 0 || c.Media.InlineImageMaxMB > 5 || c.Media.VoiceMaxSeconds <= 0 || c.Media.VoiceMaxSeconds > 60 || c.QueueLimit < 1 {
		return fmt.Errorf("invalid media or queue limits")
	}
	for _, s := range c.Stream.Separators {
		if s == "" {
			return fmt.Errorf("stream separators must not be empty")
		}
	}
	if len(c.Frontends) != 1 || c.Frontends["telegram"].Type != "telegram" {
		return fmt.Errorf("this build requires one frontend named telegram of type telegram")
	}
	if len(c.Backends) == 0 || len(c.Routes) == 0 {
		return fmt.Errorf("backends and routes are required")
	}
	for name, b := range c.Backends {
		if b.Type != "pi" || len(b.Command) == 0 || b.Command[0] == "" {
			return fmt.Errorf("invalid backend %s", name)
		}
	}
	seen := map[string]bool{}
	for _, r := range c.Routes {
		kind, name, ok := strings.Cut(r.To, ":")
		b, exists := c.Backends[name]
		if !ok || !exists || kind != b.Type {
			return fmt.Errorf("unknown route destination %s", r.To)
		}
		f, chat, ok := strings.Cut(r.From, ":")
		if !ok || f != "telegram" || chat == "" {
			return fmt.Errorf("invalid route source %s", r.From)
		}
		if chat != "*" {
			if _, err := strconv.ParseInt(chat, 10, 64); err != nil {
				return fmt.Errorf("invalid chat ID in route")
			}
		}
		if seen[r.From] {
			return fmt.Errorf("duplicate route source %s", r.From)
		}
		seen[r.From] = true
	}
	for _, p := range c.Plugins {
		switch p.Type {
		case "prefix":
		case "ticker":
			if p.Interval <= 0 || p.Text == "" {
				return fmt.Errorf("ticker needs interval and text")
			}
			found := false
			for _, r := range c.Routes {
				found = found || r.To == p.Route
			}
			if !found {
				return fmt.Errorf("ticker route is not configured")
			}
		default:
			return fmt.Errorf("unknown plugin type %s", p.Type)
		}
	}
	return nil
}
