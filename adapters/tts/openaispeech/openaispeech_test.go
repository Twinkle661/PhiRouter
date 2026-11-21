package openaispeech

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSynthesizeRequestShape(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/audio/speech" || r.Method != "POST" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("ID3audio"))
	}))
	defer srv.Close()
	o := DefaultOptions()
	o.BaseURL, o.APIKey, o.Model, o.Voice, o.StyleField = srv.URL+"/api/v1/", "key", "openai/gpt-4o-mini-tts-2025-12-15", "nova", "provider.options.openai.instructions"
	o.ExtraBody = map[string]any{"provider": map[string]any{"options": map[string]any{"deepinfra": map[string]any{"x": 1}}}, "input": "must be overwritten"}
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	audio, ext, err := New(*o).Synthesize(context.Background(), "hello", "gentle")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(audio)
	_ = audio.Close()
	if string(data) != "ID3audio" || ext != ".mp3" || auth != "Bearer key" {
		t.Fatal(string(data), ext, auth)
	}
	options := got["provider"].(map[string]any)["options"].(map[string]any)
	if got["input"] != "hello" || got["voice"] != "nova" || got["response_format"] != "mp3" || options["openai"].(map[string]any)["instructions"] != "gentle" || options["deepinfra"].(map[string]any)["x"] != float64(1) {
		t.Fatalf("unexpected body: %v", got)
	}
	if _, ok := got["speed"]; ok {
		t.Fatal("zero speed should be omitted")
	}
	// Without style, the style path is absent.
	o.ExtraBody, o.StyleField, o.Speed = nil, "instructions", 1.5
	if _, _, err = New(*o).Synthesize(context.Background(), "hi", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["instructions"]; ok || got["speed"] != 1.5 {
		t.Fatalf("unexpected body: %v", got)
	}
}

func TestSynthesizeErrors(t *testing.T) {
	status, ctype, body := 401, "application/json", `{"error":{"message":"No auth credentials found","code":401}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ctype)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	o := DefaultOptions()
	o.BaseURL, o.Model, o.Voice = srv.URL, "m", "v"
	p := New(*o)
	for _, c := range []struct {
		status      int
		ctype, body string
		want        string
	}{
		{401, "application/json", `{"error":{"message":"No auth credentials found"}}`, "401: No auth credentials found"},
		{422, "application/json", `{"detail":"bad voice"}`, "422: bad voice"},
		{200, "application/json", `{"error":"quota"}`, "200: quota"},
		{502, "text/plain", "upstream down", "502: upstream down"},
	} {
		status, ctype, body = c.status, c.ctype, c.body
		_, _, err := p.Synthesize(context.Background(), "x", "")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want %q, got %v", c.want, err)
		}
	}
	o.StyleField = "provider.options"
	o.ExtraBody = map[string]any{"provider": "flat"}
	if _, _, err := New(*o).Synthesize(context.Background(), "x", "calm"); err == nil {
		t.Error("style path through a non-object accepted")
	}
}

func TestValidate(t *testing.T) {
	valid := func() *Options {
		o := DefaultOptions()
		o.Model, o.Voice = "m", "v"
		return o
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Options){
		"base url":    func(o *Options) { o.BaseURL = "ftp://x" },
		"model":       func(o *Options) { o.Model = " " },
		"voice":       func(o *Options) { o.Voice = "" },
		"pcm":         func(o *Options) { o.ResponseFormat = "pcm" },
		"wav":         func(o *Options) { o.ResponseFormat = "wav" },
		"speed":       func(o *Options) { o.Speed = 9 },
		"empty key":   func(o *Options) { o.StyleField = "a..b" },
		"overwrite":   func(o *Options) { o.StyleField = "input" },
		"no timeout":  func(o *Options) { o.Timeout = 0 },
		"empty field": func(o *Options) { o.StyleField = "" },
	} {
		o := valid()
		mutate(o)
		if o.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
