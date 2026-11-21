package main

import (
	"fmt"
	"github.com/Twinkle661/PhiRouter/adapters/backend/acp"
	"github.com/Twinkle661/PhiRouter/adapters/backend/pi"
	"github.com/Twinkle661/PhiRouter/internal/config"
	"github.com/Twinkle661/PhiRouter/internal/core"
	"github.com/Twinkle661/PhiRouter/internal/router"
	"path/filepath"
)

// backendEnv is what the router provides to every backend instance.
type backendEnv struct {
	Name string
	// StateDir is the instance's own persistent directory.
	StateDir string
	// Tools is the router's MCP endpoint, addressed to this instance.
	Tools core.ToolServer
	Media core.MediaStore
}

// backendType pairs a backend adapter's configuration contract with its
// constructor, like frontendType.
type backendType struct {
	config.BackendType
	build func(options any, env backendEnv) (core.Backend, error)
}

// backendTypes is the set of backend adapters compiled into this binary.
// Adding a backend means adding an entry here; core and router stay unchanged.
var backendTypes = map[string]backendType{
	"pi": {
		BackendType: config.BackendType{NewOptions: func() any { return pi.DefaultConfig() }},
		build: func(options any, env backendEnv) (core.Backend, error) {
			o, ok := options.(*pi.Config)
			if !ok {
				return nil, fmt.Errorf("pi options missing")
			}
			return pi.FromConfig(o, env.StateDir, env.Tools, env.Media)
		},
	},
	"acp": {
		BackendType: config.BackendType{NewOptions: func() any { return acp.DefaultConfig() }},
		build: func(options any, env backendEnv) (core.Backend, error) {
			o, ok := options.(*acp.Config)
			if !ok {
				return nil, fmt.Errorf("acp options missing")
			}
			return acp.FromConfig(o, env.StateDir, env.Tools, env.Media), nil
		},
	},
}

func backendRegistry() config.BackendRegistry {
	reg := config.BackendRegistry{}
	for name, t := range backendTypes {
		reg[name] = t.BackendType
	}
	return reg
}

// registries collects every adapter type compiled into this binary.
func registries() config.Registries {
	return config.Registries{Frontends: frontendRegistry(), Backends: backendRegistry(), Providers: providerRegistry()}
}

// buildBackends constructs every configured instance with its busy policy.
func buildBackends(c config.Config, store core.MediaStore) (map[string]core.Backend, map[string]router.BusyPolicy, error) {
	backends := map[string]core.Backend{}
	policies := map[string]router.BusyPolicy{}
	endpoint := "http://" + c.MCP.Listen + "/mcp"
	for name, b := range c.Backends {
		t, ok := backendTypes[b.Type]
		if !ok {
			return nil, nil, fmt.Errorf("backend %s: unknown type %q", name, b.Type)
		}
		env := backendEnv{Name: name, StateDir: filepath.Join(c.State.Dir, name), Tools: core.ToolServer{URL: endpoint, Token: c.MCP.Token, Route: name}, Media: store}
		built, err := t.build(b.Options, env)
		if err != nil {
			return nil, nil, fmt.Errorf("backend %s: %w", name, err)
		}
		backends[name] = built
		if b.BusyPolicy == "interrupt" {
			policies[name] = router.BusyInterrupt
		}
	}
	return backends, policies, nil
}
