package execd

import (
	"net/http/httptest"
	"testing"
)

func TestAuthenticateRootToken(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Tokens = []TokenConfig{{ID: "root", SHA256: SHA256Hex("secret"), AllowRoot: true}}
	r := httptest.NewRequest("POST", "/v1/run", nil)
	r.Header.Set("Authorization", "Bearer secret")
	auth, err := authenticate(r, cfg)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if auth.TokenID != "root" || !auth.AllowRoot {
		t.Fatalf("unexpected auth info: %+v", auth)
	}
}
