package api

import (
	"encoding/json"
	"net/http"
)

func decodeJSON[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		writeError(w, 400, "invalid JSON")
		return v, false
	}
	return v, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
