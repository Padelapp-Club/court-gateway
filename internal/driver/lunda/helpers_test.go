package lunda_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
)

// recorder serves h and keeps the body of its last answer in *last.
func recorder(h http.Handler, last *[]byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		*last = bytes.Clone(rec.Body.Bytes())
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}
