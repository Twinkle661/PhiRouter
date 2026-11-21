package config

import (
	"bufio"
	"bytes"
	"fmt"
	"gopkg.in/yaml.v3"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Keep IANA timezones available in minimal containers.
)

type Endpoint struct {
	Listen string `yaml:"listen"`
	Token  string `yaml:"token"`
}
type Timestamp struct {
	Enabled  bool   `yaml:"enabled"`
	Timezone string `yaml:"timezone"`
	Format   string `yaml:"format"`
	// Location is resolved from Timezone by Load when Enabled; not configurable.
	Location *time.Location `yaml:"-"`
}

type Config struct {
	Timestamp Timestamp           `yaml:"timestamp"`
	Debug     bool                `yaml:"debug"`
	Frontends map[string]Frontend `yaml:"frontends"`
	Backends  map[string]Backend  `yaml:"backends"`
	Routes    []Route             `yaml:"routes"`
	// LifecycleNotify lists frontend instances whose owner receives startup
	// and shutdown notices.
	LifecycleNotify []string `yaml:"lifecycle_notify"`
	Stream          struct {
		Separators    []string  `yaml:"separators"`
		ShowToolCalls ToolCalls `yaml:"show_tool_calls"`
	} `yaml:"stream"`
	Media struct {
		Dir string        `yaml:"dir"`
		TTL time.Duration `yaml:"ttl"`
	} `yaml:"media"`
	// State holds per-backend state directories, <dir>/<backend name>. A
	// relative dir is resolved against the configuration file's directory.
	State struct {
		Dir string `yaml:"dir"`
	} `yaml:"state"`
	API Endpoint `yaml:"api"`
	MCP MCP      `yaml:"mcp"`
	// Ingress serves webhook endpoints for frontends that declare an
	// ingress path. It may bind publicly and only starts when used.
	Ingress struct {
		Listen string `yaml:"listen"`
	} `yaml:"ingress"`
	Plugins    []Plugin `yaml:"plugins"`
	QueueLimit int      `yaml:"queue_limit"`
}

// MCP configures the built-in MCP server and its optional tools.
type MCP struct {
	Listen string `yaml:"listen"`
	Token  string `yaml:"token"`
	Tools  struct {
		SendVoice SendVoice `yaml:"send_voice"`
	} `yaml:"tools"`
}

// SendVoice configures the send_voice tool: text is synthesized by Provider
// and delivered to the user as audio.
type SendVoice struct {
	Enabled  bool     `yaml:"enabled"`
	Provider Provider `yaml:"provider"`
	// Style lets the agent describe the delivery (emotion, tone) of each
	// message. The agent's text is appended to Base; the provider decides
	// where the combined text goes in its request.
	Style struct {
		Enabled bool   `yaml:"enabled"`
		Base    string `yaml:"base"`
	} `yaml:"style"`
}

// Provider is a typed adapter instance like Frontend: Options holds a pointer
// to the options struct registered for Type, decoded strictly by Load.
type Provider struct {
	Type    string `yaml:"type"`
	Options any    `yaml:"options"`
	raw     *yaml.Node
}

func (p *Provider) UnmarshalYAML(n *yaml.Node) error {
	aux := struct {
		Type    string    `yaml:"type"`
		Options yaml.Node `yaml:"options"`
	}{Type: p.Type}
	if err := strict(n, &aux); err != nil {
		return err
	}
	p.Type, p.Options, p.raw = aux.Type, nil, nil
	if aux.Options.Kind != 0 {
		p.raw = &aux.Options
	}
	return nil
}

// ProviderType describes a provider adapter's options contract. NewOptions
// returns a pointer to its options struct filled with defaults; if that has a
// Validate() error method, Validate calls it.
type ProviderType struct {
	NewOptions func() any
}
type ProviderRegistry map[string]ProviderType

// Frontend is one frontend instance. Options holds a pointer to the options
// struct registered for Type; Load decodes it strictly.
type Frontend struct {
	Type    string `yaml:"type"`
	Owner   string `yaml:"owner"`
	Ingress struct {
		Path string `yaml:"path"`
	} `yaml:"ingress"`
	Options any `yaml:"options"`
	raw     *yaml.Node
}

func (f *Frontend) UnmarshalYAML(n *yaml.Node) error {
	var aux struct {
		Type    string `yaml:"type"`
		Owner   string `yaml:"owner"`
		Ingress struct {
			Path string `yaml:"path"`
		} `yaml:"ingress"`
		Options yaml.Node `yaml:"options"`
	}
	if err := strict(n, &aux); err != nil {
		return err
	}
	f.Type, f.Owner, f.Ingress.Path = aux.Type, aux.Owner, aux.Ingress.Path
	f.Options, f.raw = nil, nil
	if aux.Options.Kind != 0 {
		f.raw = &aux.Options
	}
	return nil
}

// FrontendType describes a frontend adapter to the configuration layer
// without importing it. The composition root supplies a Registry.
type FrontendType struct {
	// NewOptions returns a pointer to the adapter's options struct, filled
	// with defaults. If it has a Validate() error method, Validate calls it.
	NewOptions func() any
	// ValidAddress checks a concrete (non-wildcard) address.
	ValidAddress func(string) error
}
type Registry map[string]FrontendType

func strict(n *yaml.Node, out any) error {
	raw, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	d := yaml.NewDecoder(bytes.NewReader(raw))
	d.KnownFields(true)
	return d.Decode(out)
}

// ToolCalls controls tool notices: a start notice per call and a notice when
// a call fails. Detailed appends an argument summary (such as the shell
// command) to start notices, clipped to MaxDetailLength runes.
type ToolCalls struct {
	Enabled         bool `yaml:"enabled"`
	Detailed        bool `yaml:"detailed"`
	MaxDetailLength int  `yaml:"max_detail_length"`
}

// UnmarshalYAML also accepts the earlier boolean form, show_tool_calls: true.
func (t *ToolCalls) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&t.Enabled)
	}
	type plain ToolCalls
	return strict(n, (*plain)(t))
}

// Backend is one backend instance. Options holds a pointer to the options
// struct registered for Type; Load decodes it strictly.
type Backend struct {
	Type string `yaml:"type"`
	// BusyPolicy is "queue" (default) or "interrupt": what a user message
	// does while a backend that cannot steer is busy.
	BusyPolicy string `yaml:"busy_policy"`
	Options    any    `yaml:"options"`
	raw        *yaml.Node
}

func (b *Backend) UnmarshalYAML(n *yaml.Node) error {
	var aux struct {
		Type       string    `yaml:"type"`
		BusyPolicy string    `yaml:"busy_policy"`
		Options    yaml.Node `yaml:"options"`
	}
	if err := strict(n, &aux); err != nil {
		return err
	}
	b.Type, b.BusyPolicy, b.Options, b.raw = aux.Type, aux.BusyPolicy, nil, nil
	if aux.Options.Kind != 0 {
		b.raw = &aux.Options
	}
	return nil
}

// BackendType describes a backend adapter's options contract. NewOptions
// returns a pointer to its options struct filled with defaults; if that has
// a Validate() error method, Validate calls it.
type BackendType struct {
	NewOptions func() any
}
type BackendRegistry map[string]BackendType

// Registries are the adapter types compiled into the binary, supplied by the
// composition root so configuration never imports adapters.
type Registries struct {
	Frontends Registry
	Backends  BackendRegistry
	Providers ProviderRegistry
}

// DefaultBackend is used when the configuration defines no backends.
const DefaultBackend = "default"
const defaultBackendType = "pi"

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
	c.Timestamp.Timezone = "UTC"
	c.Timestamp.Format = "[2006-01-02 15:04:05 MST] "
	c.Stream.Separators = []string{"\n", "<break />"}
	c.Stream.ShowToolCalls.Detailed = true
	c.Stream.ShowToolCalls.MaxDetailLength = 120
	c.Media.Dir = "/data/media"
	c.State.Dir = "state"
	c.Media.TTL = 24 * time.Hour
	c.API.Listen = "127.0.0.1:3710"
	c.MCP.Listen = "127.0.0.1:8081"
	c.MCP.Tools.SendVoice.Provider.Type = "openai_speech"
	c.QueueLimit = 256
	return c
}
func Load(path, dotenv string, reg Registries) (Config, error) {
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
	// Adapter options are typed by their instance's type, which may itself
	// come from the environment, so they are materialized between two passes.
	isOptions := func(path []string) bool {
		frontend := len(path) >= 3 && (path[0] == "frontends" || path[0] == "backends") && path[2] == "options"
		provider := len(path) >= 5 && path[0] == "mcp" && path[1] == "tools" && path[3] == "provider" && path[4] == "options"
		return frontend || provider
	}
	for _, pass := range []bool{false, true} {
		if pass {
			if len(c.Backends) == 0 {
				c.Backends = map[string]Backend{DefaultBackend: {Type: defaultBackendType}}
			}
			if err = c.materialize(reg); err != nil {
				return c, err
			}
		}
		for _, k := range keys {
			path := strings.Split(strings.ToLower(strings.TrimPrefix(k, "ROUTER_")), "__")
			if isOptions(path) != pass {
				continue
			}
			if err = set(reflect.ValueOf(&c).Elem(), path, vars[k]); err != nil {
				return c, fmt.Errorf("%s: %w", k, err)
			}
		}
	}
	if err = c.Validate(reg); err != nil {
		return c, err
	}
	if !filepath.IsAbs(c.State.Dir) {
		c.State.Dir = filepath.Join(filepath.Dir(path), c.State.Dir)
	}
	if c.Timestamp.Enabled {
		// Validate has already checked the zone, so this cannot fail.
		c.Timestamp.Location, _ = time.LoadLocation(c.Timestamp.Timezone)
	}
	return c, nil
}

// materialize decodes each adapter instance's options into its registered type.
func (c *Config) materialize(reg Registries) error {
	if p := &c.MCP.Tools.SendVoice.Provider; p.Options == nil && p.Type != "" {
		t, ok := reg.Providers[p.Type]
		switch {
		case ok:
			p.Options = t.NewOptions()
			if p.raw != nil {
				if err := strict(p.raw, p.Options); err != nil {
					return fmt.Errorf("mcp.tools.send_voice.provider options: %w", err)
				}
				p.raw = nil
			}
		case c.MCP.Tools.SendVoice.Enabled || p.raw != nil:
			return fmt.Errorf("mcp.tools.send_voice.provider: unknown type %q", p.Type)
		}
	}
	for name, f := range c.Frontends {
		if f.Options != nil {
			continue
		}
		t, ok := reg.Frontends[f.Type]
		if !ok {
			return fmt.Errorf("frontend %s: unknown type %q", name, f.Type)
		}
		f.Options = t.NewOptions()
		if f.raw != nil {
			if err := strict(f.raw, f.Options); err != nil {
				return fmt.Errorf("frontend %s options: %w", name, err)
			}
			f.raw = nil
		}
		c.Frontends[name] = f
	}
	for name, b := range c.Backends {
		if b.Options != nil {
			continue
		}
		t, ok := reg.Backends[b.Type]
		if !ok {
			return fmt.Errorf("backend %s: unknown type %q", name, b.Type)
		}
		b.Options = t.NewOptions()
		if b.raw != nil {
			if err := strict(b.raw, b.Options); err != nil {
				return fmt.Errorf("backend %s options: %w", name, err)
			}
			b.raw = nil
		}
		c.Backends[name] = b
	}
	return nil
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
	// Materialized options: descend into the concrete struct.
	if v.Kind() == reflect.Interface && !v.IsNil() && v.Elem().Kind() == reflect.Pointer {
		return set(v.Elem().Elem(), path, raw)
	}
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

var instanceName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
var ingressPath = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+$`)

func (c Config) Validate(regs Registries) error {
	reg, providers := regs.Frontends, regs.Providers
	if c.Timestamp.Enabled {
		if strings.TrimSpace(c.Timestamp.Timezone) == "" || strings.TrimSpace(c.Timestamp.Format) == "" {
			return fmt.Errorf("timestamp.timezone and timestamp.format must not be empty when enabled")
		}
		if _, err := time.LoadLocation(c.Timestamp.Timezone); err != nil {
			return fmt.Errorf("invalid timestamp.timezone: %w", err)
		}
	}
	if c.API.Token == "" || c.MCP.Token == "" || c.API.Token == c.MCP.Token {
		return fmt.Errorf("api.token and mcp.token must be nonempty and different")
	}
	for name, ep := range map[string]Endpoint{"api": c.API, "mcp": {Listen: c.MCP.Listen, Token: c.MCP.Token}} {
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
	if c.Ingress.Listen != "" {
		_, p, err := net.SplitHostPort(c.Ingress.Listen)
		if port, e := strconv.Atoi(p); err != nil || e != nil || port < 1 || port > 65535 {
			return fmt.Errorf("invalid ingress.listen")
		}
		if c.Ingress.Listen == c.API.Listen || c.Ingress.Listen == c.MCP.Listen {
			return fmt.Errorf("ingress listener must differ from API and MCP")
		}
	}
	if c.Media.Dir == "" || c.Media.TTL <= 0 || c.QueueLimit < 1 {
		return fmt.Errorf("invalid media or queue limits")
	}
	if c.State.Dir == "" {
		return fmt.Errorf("state.dir must not be empty")
	}
	if c.Stream.ShowToolCalls.MaxDetailLength < 1 {
		return fmt.Errorf("stream.show_tool_calls.max_detail_length must be at least 1")
	}
	for _, s := range c.Stream.Separators {
		if s == "" {
			return fmt.Errorf("stream separators must not be empty")
		}
	}
	if v := c.MCP.Tools.SendVoice; v.Enabled {
		if _, ok := providers[v.Provider.Type]; !ok {
			return fmt.Errorf("mcp.tools.send_voice.provider: unknown type %q", v.Provider.Type)
		}
		if o, ok := v.Provider.Options.(interface{ Validate() error }); ok {
			if err := o.Validate(); err != nil {
				return fmt.Errorf("mcp.tools.send_voice.provider options: %w", err)
			}
		}
	}
	if len(c.Frontends) == 0 {
		return fmt.Errorf("at least one frontend is required")
	}
	paths := map[string]string{}
	for name, f := range c.Frontends {
		if !instanceName.MatchString(name) {
			return fmt.Errorf("frontend name %q must use lowercase letters, digits, - or _", name)
		}
		t, ok := reg[f.Type]
		if !ok {
			return fmt.Errorf("frontend %s: unknown type %q", name, f.Type)
		}
		if f.Owner == "" {
			return fmt.Errorf("frontend %s: owner is required", name)
		}
		if err := t.ValidAddress(f.Owner); err != nil {
			return fmt.Errorf("frontend %s owner: %w", name, err)
		}
		if v, ok := f.Options.(interface{ Validate() error }); ok {
			if err := v.Validate(); err != nil {
				return fmt.Errorf("frontend %s options: %w", name, err)
			}
		}
		if p := f.Ingress.Path; p != "" {
			if !ingressPath.MatchString(p) {
				return fmt.Errorf("frontend %s: invalid ingress path %q", name, p)
			}
			if other, dup := paths[p]; dup {
				return fmt.Errorf("frontends %s and %s share ingress path %s", other, name, p)
			}
			paths[p] = name
			if c.Ingress.Listen == "" {
				return fmt.Errorf("frontend %s uses ingress but ingress.listen is empty", name)
			}
		}
	}
	notify := map[string]bool{}
	for _, name := range c.LifecycleNotify {
		if _, ok := c.Frontends[name]; !ok || notify[name] {
			return fmt.Errorf("lifecycle_notify: unknown or duplicate frontend %q", name)
		}
		notify[name] = true
	}
	if len(c.Backends) == 0 || len(c.Routes) == 0 {
		return fmt.Errorf("backends and routes are required")
	}
	for name, b := range c.Backends {
		if !instanceName.MatchString(name) {
			return fmt.Errorf("backend name %q must use lowercase letters, digits, - or _", name)
		}
		if _, ok := regs.Backends[b.Type]; !ok {
			return fmt.Errorf("backend %s: unknown type %q", name, b.Type)
		}
		if b.BusyPolicy != "" && b.BusyPolicy != "queue" && b.BusyPolicy != "interrupt" {
			return fmt.Errorf("backend %s: busy_policy must be queue or interrupt", name)
		}
		if v, ok := b.Options.(interface{ Validate() error }); ok {
			if err := v.Validate(); err != nil {
				return fmt.Errorf("backend %s options: %w", name, err)
			}
		}
	}
	seen := map[string]bool{}
	for _, r := range c.Routes {
		if _, exists := c.Backends[r.To]; !exists {
			return fmt.Errorf("unknown route destination %q (routes name a backend)", r.To)
		}
		name, address, ok := strings.Cut(r.From, ":")
		front, exists := c.Frontends[name]
		if !ok || !exists || address == "" {
			return fmt.Errorf("invalid route source %s", r.From)
		}
		if address != "*" {
			if err := reg[front.Type].ValidAddress(address); err != nil {
				return fmt.Errorf("route source %s: %w", r.From, err)
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
