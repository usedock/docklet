package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/ids"
)

// TokenFile is the state-dir file holding the local API bearer token.
const TokenFile = "local-api-token"

// NewToken returns the configured token, or a fresh random one when preset is empty, and writes it 0600 to dir.
func NewToken(dir, preset string) (string, error) {
	token := strings.TrimSpace(preset)
	if token == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		token = hex.EncodeToString(raw)
	}
	path := filepath.Join(dir, TokenFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return token, nil
}

// auth requires the local bearer token; an unset token refuses everything.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.deps.Token == "" || !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.deps.Token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, apiv1.Envelope{Ok: false, Code: "unauthorized", Message: "local api token required"})
			return
		}
		next(w, r)
	}
}

// badID answers 400 and reports true when a required id is unsafe; optional ids may be empty.
func badID(w http.ResponseWriter, kind, id string, optional bool) bool {
	if optional && id == "" {
		return false
	}
	if err := ids.Check(kind, id); err != nil {
		writeJSON(w, http.StatusBadRequest, apiv1.Envelope{Ok: false, Code: "validation_error", Message: err.Error()})
		return true
	}
	return false
}
