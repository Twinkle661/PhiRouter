package telegram

import (
	"PhiRouter/internal/core"
	"PhiRouter/internal/media"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAuthorizationDialogsAndMedia(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var token string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		calls = append(calls, method)
		if strings.Contains(r.URL.Path, "/file/") {
			_, _ = w.Write([]byte("photo"))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "sendMessage":
			if markup, ok := body["reply_markup"].(map[string]any); ok {
				if rows, ok := markup["inline_keyboard"].([]any); ok {
					token = rows[0].([]any)[0].(map[string]any)["callback_data"].(string)
				}
			}
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":10}}`)
		case "getFile":
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"file_path":"photos/image.jpg","file_size":5}}`)
		default:
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
	}))
	defer server.Close()
	store, err := media.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	f := New(Options{Token: "test", UserID: 42, BaseURL: server.URL, Media: store})
	var received []core.Envelope
	receive := func(_ context.Context, e core.Envelope) error { received = append(received, e); return nil }
	ctx := context.Background()
	decode := func(raw string) update {
		var u update
		if err = json.Unmarshal([]byte(raw), &u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	if err = f.handle(ctx, decode(`{"message":{"from":{"id":99},"chat":{"id":99},"text":"not allowed","document":{"file_id":"secret"}}}`), receive); err != nil {
		t.Fatal(err)
	}
	if len(received) != 0 || len(calls) != 0 {
		t.Fatal("unauthorized update touched network")
	}
	if err = f.Dialog(ctx, "42", core.Dialog{ID: "request1", Method: "select", Title: "Choose", Options: []string{strings.Repeat("long", 100)}}); err != nil {
		t.Fatal(err)
	}
	if len(token) > 64 {
		t.Fatal("oversized callback")
	}
	callback := fmt.Sprintf(`{"callback_query":{"id":"q","from":{"id":42},"data":%q,"message":{"message_id":10,"chat":{"id":42}}}}`, token)
	if err = f.handle(ctx, decode(callback), receive); err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 || received[0].Reply == nil || received[0].Reply.ID != "request1" {
		t.Fatal(received)
	}
	_ = f.handle(ctx, decode(callback), receive)
	if len(received) != 1 {
		t.Fatal("callback replay accepted")
	}
	if err = f.handle(ctx, decode(`{"message":{"from":{"id":42},"chat":{"id":42},"photo":[{"file_id":"photo","file_size":5}],"caption":"Look"}}`), receive); err != nil {
		t.Fatal(err)
	}
	e := received[1]
	if len(e.Parts) != 2 || e.Parts[1].Type != "image" || e.Parts[1].Size != 5 {
		t.Fatal(e)
	}
	if err = f.handle(ctx, decode(`{"message":{"from":{"id":42},"chat":{"id":42},"document":{"file_id":"large","file_size":20971521}}}`), receive); err == nil {
		t.Fatal("large file accepted")
	}
}
func TestForceReplyIsNotPrompt(t *testing.T) {
	f := New(Options{Token: "test", UserID: 42})
	f.replies[10] = answer{reply: core.Reply{ID: "input1"}, chat: 42, message: 10}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, `{"ok":true,"result":true}`) }))
	defer server.Close()
	f.opt.BaseURL = server.URL
	var u update
	_ = json.Unmarshal([]byte(`{"message":{"from":{"id":42},"chat":{"id":42},"text":"answer","reply_to_message":{"message_id":10}}}`), &u)
	var got core.Envelope
	err := f.handle(context.Background(), u, func(_ context.Context, e core.Envelope) error { got = e; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got.Reply == nil || got.Reply.Value != "answer" || len(got.Parts) != 0 {
		t.Fatal(got)
	}
}

func TestVoiceDetectionWithFFprobe(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}
	store, err := media.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	f := New(Options{Media: store, VoiceMaxSeconds: 60})
	for _, tc := range []struct {
		name, codec, duration string
		voice                 bool
	}{{"short.ogg", "libopus", "1", true}, {"short.mp3", "libmp3lame", "1", true}, {"short.m4a", "aac", "1", true}, {"long.ogg", "libopus", "61", false}, {"short.wav", "pcm_s16le", "1", false}} {
		path := filepath.Join(store.Dir, "out", tc.name)
		cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=mono", "-t", tc.duration, "-c:a", tc.codec, path)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generate %s: %v: %s", tc.name, err, output)
		}
		p, err := store.Inspect(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.voice(context.Background(), p); got != tc.voice {
			t.Errorf("%s: got voice=%t, want %t", tc.name, got, tc.voice)
		}
	}
}
