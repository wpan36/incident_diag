package lab

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// errorResponse is the body every refused request carries. It is deliberately
// not the platform's error envelope from internal/api: these are two services
// pretending to belong to someone else's estate, and sharing a response shape
// with the application that diagnoses them would be a coincidence the agent
// could learn from.
type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// The response is already committed, so a write failure — a client that
	// timed out and went away, which is most of what these services do under
	// load — has nowhere to be reported.
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// decodeRaw reads the whole body, decodes it into v, and returns the bytes so
// a second pass can decode the same body into a different type.
func decodeRaw(body io.Reader, v any) ([]byte, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("reading the request body: %w", err)
	}
	if len(raw) == 0 {
		return nil, errors.New("the request body is empty")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return nil, fmt.Errorf("the request body is not valid JSON: %w", err)
	}
	return raw, nil
}

// decodeJSON decodes a bounded request body, rejecting unknown fields so a
// misspelled field in a scenario script is a 400 rather than a silent default.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}
