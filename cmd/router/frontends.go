package main

import (
	"fmt"
	"github.com/Twinkle661/PhiRouter/adapters/frontend/telegram"
	"github.com/Twinkle661/PhiRouter/internal/config"
	"github.com/Twinkle661/PhiRouter/internal/core"
	"strconv"
)

// frontendType pairs an adapter's configuration contract with its constructor.
type frontendType struct {
	config.FrontendType
	build func(f config.Frontend, store core.MediaStore) (core.Frontend, error)
}

// frontendTypes is the set of frontend adapters compiled into this binary.
// Adding a frontend means adding an entry here; core and router stay unchanged.
var frontendTypes = map[string]frontendType{
	"telegram": {
		FrontendType: config.FrontendType{
			NewOptions:   func() any { return telegram.DefaultConfig() },
			ValidAddress: telegram.ValidAddress,
		},
		build: func(f config.Frontend, store core.MediaStore) (core.Frontend, error) {
			o, ok := f.Options.(*telegram.Config)
			if !ok {
				return nil, fmt.Errorf("telegram options missing")
			}
			owner, err := strconv.ParseInt(f.Owner, 10, 64)
			if err != nil || owner <= 0 {
				return nil, fmt.Errorf("telegram owner must be a positive user ID")
			}
			return telegram.New(telegram.Options{Token: o.Token, UserID: owner, SystemPrefix: o.SystemPrefix, SystemStyle: o.SystemStyle, VoiceMaxSeconds: o.VoiceMaxSeconds, Media: store}), nil
		},
	},
}

func frontendRegistry() config.Registry {
	reg := config.Registry{}
	for name, t := range frontendTypes {
		reg[name] = t.FrontendType
	}
	return reg
}

// buildFrontends constructs every configured instance and collects the
// webhook handlers of those that declare an ingress path.
func buildFrontends(c config.Config, store core.MediaStore) (map[string]core.Frontend, map[string]string, map[string]core.IngressFrontend, error) {
	frontends := map[string]core.Frontend{}
	owners := map[string]string{}
	ingress := map[string]core.IngressFrontend{}
	for name, f := range c.Frontends {
		t, ok := frontendTypes[f.Type]
		if !ok {
			return nil, nil, nil, fmt.Errorf("frontend %s: unknown type %q", name, f.Type)
		}
		built, err := t.build(f, store)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("frontend %s: %w", name, err)
		}
		if f.Ingress.Path != "" {
			in, ok := built.(core.IngressFrontend)
			if !ok {
				return nil, nil, nil, fmt.Errorf("frontend %s: type %s does not support ingress", name, f.Type)
			}
			ingress[f.Ingress.Path] = in
		}
		frontends[name] = built
		owners[name] = f.Owner
	}
	return frontends, owners, ingress, nil
}
