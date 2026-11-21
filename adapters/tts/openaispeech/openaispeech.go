// Package openaispeech synthesizes speech through any endpoint that speaks
// the OpenAI audio/speech protocol: OpenRouter, OpenAI, DeepInfra, or a
// self-hosted server. Provider differences are configuration (base URL,
// extra body fields, where style text goes), not code.
package openaispeech

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Options is the provider's configuration contract, decoded strictly from
// mcp.tools.send_voice.provider.options.
type Options struct {
	// BaseURL is the API root; "/audio/speech" is appended.
	BaseURL string `yaml:"base_url"`
	// APIKey is sent as a bearer token. Leave it empty for servers that need
	// no authentication.
	APIKey string `yaml:"api_key"`
	Model  string `yaml:"model"`
	Voice  string `yaml:"voice"`
	// ResponseFormat must be one Telegram plays as a voice message.
	ResponseFormat string `yaml:"response_format"`
	// Speed is omitted from the request when zero.
	Speed float64 `yaml:"speed"`
	// StyleField is the dot-separated body path that receives style text.
	// OpenAI and OpenRouter use "instructions"; a nested path such as
	// "provider.options.<slug>.x" reaches provider-specific fields.
	StyleField string `yaml:"style_field"`
	// ExtraBody is merged into every request body. Standard fields win.
	ExtraBody map[string]any `yaml:"extra_body"`
	Timeout   time.Duration  `yaml:"timeout"`
}

// DefaultOptions targets OpenRouter.
func DefaultOptions() *Options {
	return &Options{
		BaseURL:        "https://openrouter.ai/api/v1",
		ResponseFormat: "mp3",
		StyleField:     "instructions",
		Timeout:        60 * time.Second,
	}
}

var extensions = map[string]string{"mp3": ".mp3", "opus": ".opus"}

func (o *Options) Validate() error {
	u, err := url.Parse(o.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("base_url must be an http(s) URL")
	}
	if strings.TrimSpace(o.Model) == "" || strings.TrimSpace(o.Voice) == "" {
		return errors.New("model and voice are required")
	}
	if _, ok := extensions[o.ResponseFormat]; !ok {
		return errors.New("response_format must be mp3 or opus")
	}
	if o.Speed != 0 && (o.Speed < 0.25 || o.Speed > 4) {
		return errors.New("speed must be 0 (omitted) or between 0.25 and 4")
	}
	if _, err := splitField(o.StyleField); err != nil {
		return err
	}
	if o.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	return nil
}

func splitField(field string) ([]string, error) {
	keys := strings.Split(field, ".")
	for _, k := range keys {
		if k == "" {
			return nil, fmt.Errorf("style_field %q must be a dot-separated path of nonempty keys", field)
		}
	}
	switch keys[0] {
	case "model", "input", "voice", "response_format", "speed":
		return nil, fmt.Errorf("style_field must not overwrite %q", keys[0])
	}
	return keys, nil
}

// maxAudio bounds a response; Telegram's bot upload limit is 50 MB.
const maxAudio = 50 << 20

type Provider struct {
	opt    Options
	client *http.Client
}

func New(o Options) *Provider {
	return &Provider{opt: o, client: &http.Client{Timeout: o.Timeout}}
}

// Synthesize returns the audio stream and its file extension. style is
// written to StyleField when nonempty.
func (p *Provider) Synthesize(ctx context.Context, text, style string) (io.ReadCloser, string, error) {
	body, err := p.body(text, style)
	if err != nil {
		return nil, "", err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(p.opt.BaseURL, "/")+"/audio/speech", bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.opt.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.opt.APIKey)
	}
	res, err := p.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("speech request failed: %w", err)
	}
	if res.StatusCode/100 != 2 || strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		defer res.Body.Close()
		return nil, "", fmt.Errorf("speech provider returned %d: %s", res.StatusCode, errorMessage(res.Body))
	}
	return limited{io.LimitReader(res.Body, maxAudio), res.Body}, extensions[p.opt.ResponseFormat], nil
}

func (p *Provider) body(text, style string) (map[string]any, error) {
	body := map[string]any{}
	if p.opt.ExtraBody != nil {
		// Round-trip through JSON so nested maps are fresh and mergeable.
		raw, err := json.Marshal(p.opt.ExtraBody)
		if err != nil {
			return nil, fmt.Errorf("extra_body: %w", err)
		}
		if err = json.Unmarshal(raw, &body); err != nil {
			return nil, fmt.Errorf("extra_body: %w", err)
		}
	}
	body["model"], body["input"], body["voice"], body["response_format"] = p.opt.Model, text, p.opt.Voice, p.opt.ResponseFormat
	if p.opt.Speed != 0 {
		body["speed"] = p.opt.Speed
	}
	if style == "" {
		return body, nil
	}
	keys, err := splitField(p.opt.StyleField)
	if err != nil {
		return nil, err
	}
	node := body
	for _, k := range keys[:len(keys)-1] {
		next, ok := node[k].(map[string]any)
		if !ok {
			if _, exists := node[k]; exists {
				return nil, fmt.Errorf("style_field: %q in extra_body is not an object", k)
			}
			next = map[string]any{}
			node[k] = next
		}
		node = next
	}
	node[keys[len(keys)-1]] = style
	return body, nil
}

// errorMessage extracts a readable message from an error body.
func errorMessage(r io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(r, 4096))
	var e struct {
		Error  json.RawMessage `json:"error"`
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(raw, &e) == nil {
		var nested struct{ Message string }
		var flat string
		switch {
		case json.Unmarshal(e.Error, &nested) == nil && nested.Message != "":
			return nested.Message
		case json.Unmarshal(e.Error, &flat) == nil && flat != "":
			return flat
		case json.Unmarshal(e.Detail, &flat) == nil && flat != "":
			return flat
		case len(e.Detail) > 0:
			return string(e.Detail)
		}
	}
	msg := strings.TrimSpace(string(raw))
	if msg == "" {
		return "empty response"
	}
	return msg
}

type limited struct {
	io.Reader
	io.Closer
}
