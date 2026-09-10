package api

import (
	"encoding/json"
	"io"
	"net/http"
)

func decodeJSON[T any](w http.ResponseWriter, r *http.Request) (T, []byte, bool) {
	var v T

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return v, nil, false
	}

	if err := json.Unmarshal(body, &v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return v, nil, false
	}

	return v, body, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
