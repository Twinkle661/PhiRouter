package main

import (
	"fmt"
	"github.com/Twinkle661/PhiRouter/adapters/tts/openaispeech"
	"github.com/Twinkle661/PhiRouter/internal/config"
	"github.com/Twinkle661/PhiRouter/internal/core"
	"github.com/Twinkle661/PhiRouter/internal/mcp"
	"github.com/Twinkle661/PhiRouter/internal/media"
	"github.com/Twinkle661/PhiRouter/internal/tools/sendfile"
	"github.com/Twinkle661/PhiRouter/internal/tools/sendvoice"
)

// providerType pairs a speech provider's configuration contract with its
// constructor, like frontendType.
type providerType struct {
	config.ProviderType
	build func(options any) (sendvoice.Synthesizer, error)
}

// providerTypes is the set of speech providers compiled into this binary.
var providerTypes = map[string]providerType{
	"openai_speech": {
		ProviderType: config.ProviderType{NewOptions: func() any { return openaispeech.DefaultOptions() }},
		build: func(options any) (sendvoice.Synthesizer, error) {
			o, ok := options.(*openaispeech.Options)
			if !ok {
				return nil, fmt.Errorf("openai_speech options missing")
			}
			return openaispeech.New(*o), nil
		},
	},
}

func providerRegistry() config.ProviderRegistry {
	reg := config.ProviderRegistry{}
	for name, t := range providerTypes {
		reg[name] = t.ProviderType
	}
	return reg
}

// buildTools assembles the MCP tools enabled by configuration.
func buildTools(c config.Config, store *media.Store, send core.Inject) ([]mcp.Tool, error) {
	tools := []mcp.Tool{sendfile.Tool{Media: store, Send: send}}
	if v := c.MCP.Tools.SendVoice; v.Enabled {
		synth, err := providerTypes[v.Provider.Type].build(v.Provider.Options)
		if err != nil {
			return nil, fmt.Errorf("send_voice: %w", err)
		}
		tools = append(tools, sendvoice.Tool{Synth: synth, Media: store, Send: send, Style: v.Style.Enabled, StyleBase: v.Style.Base})
	}
	return tools, nil
}
