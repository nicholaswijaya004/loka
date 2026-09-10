package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// hashRequest produces a stable fingerprint of a JSON request body.
// The body is normalized before hashing — parsed into a map and re-marshalled,
// which sorts keys and strips whitespace — so semantically identical requests
// hash identically regardless of how the client serialized them.
func hashRequest(body []byte) (string, error) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("hash request: %w", err)
	}

	canonical, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("hash request: %w", err)
	}

	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
