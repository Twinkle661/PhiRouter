package api

import (
	"PhiRouter/internal/core"
	"PhiRouter/internal/media"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInjectAuthAndValidation(t *testing.T) {
	store, err := media.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	count := 0
	h := New("secret", store.Validate, func(_ context.Context, e core.Envelope) error {
		count++
		if e.Channel {
			t.Fatal("plugin gained channel priority")
		}
		return nil
	})
	for _, tc := range []struct {
		token, body string
		status      int
	}{{"", `{}`, 401}, {"secret", `{"route":"pi:default","parts":[{"type":"text","text":"hi"}]}`, 202}, {"secret", `{"route":"pi:default","parts":[{"type":"file","path":"/etc/passwd"}]}`, 400}, {"secret", `{"route":"pi:default","parts":[]}`, 400}, {"secret", `{} {}`, 400}} {
		r := httptest.NewRequest("POST", "/v1/inject", strings.NewReader(tc.body))
		r.Header.Set("Authorization", "Bearer "+tc.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("got %d: %s", w.Code, w.Body.String())
		}
	}
	if count != 1 {
		t.Fatal(count)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}
