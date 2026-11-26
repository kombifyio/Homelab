// Package localcloud admits the Cloud development issuer only for its governed
// synthetic owner. Its key is deliberately absent from general service trust.
package localcloud

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/gocommon/servicecall"
	"github.com/golang-jwt/jwt/v5"
)

const (
	KeyID      = "cloud-local-2026-11"
	PublicKey  = "G5UdneXv5fIWsUBtYDT7g_QN4qF1EIJ2v22wWbqwFuU"
	Subject    = "auth0|69dea22ac8ca3c27696f14ed" // TEST_FREE_USER
	Origin     = "http://localhost:5173"
	ReadScope  = "homelab:local-runtime:read"
	FrameScope = "homelab:local-frame"
)

var ErrDenied = errors.New("local Cloud identity denied")

func Owner(subject, tenant string) bool {
	return subject == Subject && tenant == "usr:"+Subject
}

// IsToken selects a verifier; an unverified header never admits a request.
func IsToken(raw string) bool {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[0])
	var h struct {
		Kid string `json:"kid"`
	}
	return err == nil && json.Unmarshal(b, &h) == nil && h.Kid == KeyID
}

func Key() ed25519.PublicKey {
	b, _ := base64.RawURLEncoding.DecodeString(PublicKey)
	return ed25519.PublicKey(b)
}

type Claims struct {
	jwt.RegisteredClaims
	Service        string                      `json:"svc"`
	Scope          string                      `json:"scope"`
	Owner          *servicecall.OnBehalfOf     `json:"on_behalf_of"`
	RequestBinding *servicecall.RequestBinding `json:"request_binding,omitempty"`
}

func Verify(raw, scope string) (*Claims, error) {
	return verify(raw, scope, Key(), time.Now())
}

func verify(raw, scope string, key ed25519.PublicKey, now time.Time) (*Claims, error) {
	c := &Claims{}
	t, err := jwt.ParseWithClaims(raw, c, func(t *jwt.Token) (any, error) {
		if t.Header["kid"] != KeyID {
			return nil, ErrDenied
		}
		return key, nil
	}, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer("kombify-cloud"),
		jwt.WithAudience("kombify-homelab"), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil || !t.Valid || c.IssuedAt == nil || c.ExpiresAt == nil ||
		c.ExpiresAt.Sub(c.IssuedAt.Time) <= 0 || c.ExpiresAt.Sub(c.IssuedAt.Time) > time.Minute ||
		c.Service != "cloud" || c.Scope != scope || c.Owner == nil || len(c.Owner.Roles) != 0 ||
		!Owner(c.Owner.Sub, c.Owner.OrgID) || c.RequestBinding != nil {
		return nil, ErrDenied
	}
	return c, nil
}
