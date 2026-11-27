package session

import (
	"testing"

	"github.com/kombifyio/homelab/internal/gocommon/authsession"
	"github.com/golang-jwt/jwt/v5"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func TestNewManagerDefaultsToHomelabIssuer(t *testing.T) {
	manager, err := NewManager(authsession.Config{Audience: "frontend", Secret: testSecret})
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	token, err := manager.Issue(authsession.Claims{Subject: "user-1", TenantID: "tenant-1"})
	if err != nil {
		t.Fatalf("Issue returned error: %v", err)
	}

	claims := jwt.MapClaims{}
	parsed, _, err := jwt.NewParser().ParseUnverified(token, claims)
	if err != nil || parsed == nil {
		t.Fatalf("parse token: %v", err)
	}
	if claims["iss"] != DefaultIssuer {
		t.Fatalf("issuer = %v, want %q", claims["iss"], DefaultIssuer)
	}
}

func TestNewManagerPreservesExplicitIssuer(t *testing.T) {
	manager, err := NewManager(authsession.Config{
		Issuer:   "custom",
		Audience: "frontend",
		Secret:   testSecret,
	})
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	token, err := manager.Issue(authsession.Claims{Subject: "user-1", TenantID: "tenant-1"})
	if err != nil {
		t.Fatalf("Issue returned error: %v", err)
	}

	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, claims); err != nil {
		t.Fatalf("parse token: %v", err)
	}
	if claims["iss"] != "custom" {
		t.Fatalf("issuer = %v, want custom", claims["iss"])
	}
}
