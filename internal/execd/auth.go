package execd

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

type AuthInfo struct {
	TokenID   string
	AllowRoot bool
}

func authenticate(r *http.Request, cfg Config) (AuthInfo, error) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return AuthInfo{}, errors.New("missing bearer token")
	}
	token := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	sum := sha256.Sum256([]byte(token))
	got := hex.EncodeToString(sum[:])
	for _, t := range cfg.Tokens {
		if subtle.ConstantTimeCompare([]byte(got), []byte(strings.ToLower(t.SHA256))) == 1 {
			return AuthInfo{TokenID: t.ID, AllowRoot: t.AllowRoot}, nil
		}
	}
	return AuthInfo{}, errors.New("invalid bearer token")
}
