package localcloud

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Exercise cryptographic admission and owner/purpose isolation with a disposable
// key. The deployed verifier always uses the governed pin, never this key.
func TestLocalIssuerRequiresOwnSyntheticTenantAndExactPurpose(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		name   string
		change func(jwt.MapClaims)
		denied bool
	}{
		{"owner read", func(jwt.MapClaims) {}, false},
		{"real subject", func(c jwt.MapClaims) {
			c["on_behalf_of"] = map[string]any{"sub": "auth0|real", "org_id": "usr:auth0|real"}
		}, true},
		{"real tenant", func(c jwt.MapClaims) { c["on_behalf_of"] = map[string]any{"sub": Subject, "org_id": "org_real"} }, true},
		{"missing owner", func(c jwt.MapClaims) { delete(c, "on_behalf_of") }, true},
		{"role assertion", func(c jwt.MapClaims) {
			c["on_behalf_of"] = map[string]any{"sub": Subject, "org_id": "usr:" + Subject, "roles": []string{"ADMIN"}}
		}, true},
		{"frame is not read", func(c jwt.MapClaims) { c["scope"] = FrameScope }, true},
		{"wrong audience", func(c jwt.MapClaims) { c["aud"] = "kombify-gateway" }, true},
		{"wrong issuer", func(c jwt.MapClaims) { c["iss"] = "kombify-gateway" }, true},
		{"long token", func(c jwt.MapClaims) { c["exp"] = now.Add(61 * time.Second).Unix() }, true},
		{"expired", func(c jwt.MapClaims) {
			c["iat"] = now.Add(-61 * time.Second).Unix()
			c["exp"] = now.Add(-time.Second).Unix()
		}, true},
		{"future", func(c jwt.MapClaims) { c["iat"] = now.Add(time.Second).Unix() }, true},
		{"missing iat", func(c jwt.MapClaims) { delete(c, "iat") }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := jwt.MapClaims{"iss": "kombify-cloud", "aud": "kombify-homelab", "svc": "cloud", "scope": ReadScope,
				"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "on_behalf_of": map[string]any{"sub": Subject, "org_id": "usr:" + Subject}}
			tc.change(claims)
			token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
			token.Header["kid"] = KeyID
			raw, err := token.SignedString(private)
			if err != nil {
				t.Fatal(err)
			}
			_, err = verify(raw, ReadScope, public, now)
			if (err != nil) != tc.denied {
				t.Fatalf("denied=%v, got %v", tc.denied, err)
			}
			if !tc.denied {
				if _, err := Verify(raw, ReadScope); err == nil {
					t.Fatal("test key entered the governed issuer")
				}
			}
		})
	}
}
