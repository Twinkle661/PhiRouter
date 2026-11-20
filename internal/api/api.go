package api

import (
	"PhiRouter/internal/core"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
)

type Validator func(core.Envelope) (core.Envelope, error)

func Authorized(r *http.Request, token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) == 1
}
func New(token string, validate Validator, inject core.Inject) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("POST /v1/inject", func(w http.ResponseWriter, r *http.Request) {
		if !Authorized(r, token) {
			http.Error(w, "Unauthorized", 401)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var e core.Envelope
		if err := dec.Decode(&e); err != nil {
			http.Error(w, "Invalid request body", 400)
			return
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			http.Error(w, "Expected one JSON object", 400)
			return
		}
		if e.Route == "" {
			http.Error(w, "route is required", 400)
			return
		}
		var err error
		e, err = validate(e)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err = inject(r.Context(), e); err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	return mux
}
