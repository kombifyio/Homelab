// Package session holds Homelab's V2 browser-session manager. The token
// minting and verification live in the shared [authsession] package in
// kombify-go-common; this package keeps only the Homelab-specific issuer
// default and the constructor that applies it.
//
// Migration history: this package was the original donor of authsession
// (lifted 2026-05-03 as part of the kombify auth standardization). It used to
// re-export the upstream types so old import paths kept working; those
// pass-through re-exports are gone, and callers that need only upstream
// symbols import `github.com/kombifyio/homelab/internal/gocommon/authsession`
// directly.
package session

import (
	"strings"

	"github.com/kombifyio/homelab/internal/gocommon/authsession"
)

// DefaultIssuer is the Homelab issuer value ("homelab", formerly "techstack"). Tokens
// minted via NewManager without an explicit Issuer carry iss=homelab; a verifier
// that pins the iss claim must pin this value.
const DefaultIssuer = "homelab"

// NewManager validates Config and returns a Manager. When Config.Issuer is
// empty we substitute the legacy [DefaultIssuer] ("homelab") instead of
// authsession's "kombify" default, preserving wire compatibility for the
// V2 backend.
func NewManager(cfg authsession.Config) (*authsession.Manager, error) {
	if strings.TrimSpace(cfg.Issuer) == "" {
		cfg.Issuer = DefaultIssuer
	}
	return authsession.NewManager(cfg)
}
