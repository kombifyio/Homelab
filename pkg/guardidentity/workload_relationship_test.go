package guardidentity

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWorkloadAssertionRejectsExpiredMismatchedAndReplayed(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	thumb, err := PublicJWK(key).Thumbprint()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	in := WorkloadAssertionInput{
		Issuer:            "worker-1",
		Subject:           "cloud-photos",
		WorkloadID:        "workload:cloud-photos:api",
		TenantID:          "usr:alice",
		ServiceRef:        "photos",
		Audience:          "https://photos.origin.stackkit.internal",
		Scope:             "origin:probe",
		Generation:        2,
		MaxSessionSeconds: 30,
		Lifetime:          5 * time.Minute,
		Now:               now,
	}
	token, err := SignWorkloadAssertion(key, in)
	if err != nil {
		t.Fatal(err)
	}

	expect := WorkloadAssertionExpectation{
		Issuer:                "worker-1",
		Subject:               "cloud-photos",
		WorkloadID:            "workload:cloud-photos:api",
		TenantID:              "usr:alice",
		ServiceRef:            "photos",
		Audience:              "https://photos.origin.stackkit.internal",
		Scope:                 "origin:probe",
		ExpectedKeyThumbprint: thumb,
		MinGeneration:         2,
		MaxSessionSeconds:     60,
		Eligible:              true,
		Now:                   now,
	}
	replay := NewWorkloadReplayCache()
	verified, err := VerifyWorkloadAssertion(token, expect, replay)
	if err != nil {
		t.Fatalf("valid assertion rejected: %v", err)
	}
	if verified.Thumbprint != thumb || verified.MaxSession != 30*time.Second {
		t.Fatalf("unexpected verified assertion: %+v", verified)
	}

	// Replayed token (same jti) fails closed.
	if _, err := VerifyWorkloadAssertion(token, expect, replay); err == nil {
		t.Fatal("replayed assertion was accepted")
	} else if denied, ok := err.(*Error); !ok || denied.Code != CodeProofReplayed {
		t.Fatalf("replay err = %v, want %s", err, CodeProofReplayed)
	}

	// nil replay cache fails closed instead of silently disabling replay protection.
	freshForNil, err := SignWorkloadAssertion(key, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyWorkloadAssertion(freshForNil, expect, nil); err == nil {
		t.Fatal("nil replay cache was accepted")
	} else if denied, ok := err.(*Error); !ok || denied.Code != CodeProofInvalid || denied.Reason != ReasonWorkloadAssertionReplayed {
		t.Fatalf("nil replay cache err = %v, want (%s, %s)", err, CodeProofInvalid, ReasonWorkloadAssertionReplayed)
	}

	// Established session is strictly bounded by MaxSession (30s).
	if err := CheckWorkloadSession(now, now.Add(29*time.Second), verified); err != nil {
		t.Fatalf("session within bound rejected: %v", err)
	}
	if err := CheckWorkloadSession(now, now.Add(30*time.Second), verified); err == nil {
		t.Fatal("session past bound accepted")
	}

	// Sign a fresh token for negative-case verification with a real replay cache.
	fresh, err := SignWorkloadAssertion(key, in)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, _ := GenerateKey()
	otherThumb, _ := PublicJWK(otherKey).Thumbprint()
	parts := strings.Split(fresh, ".")
	forged := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(otherKey, []byte(parts[0]+"."+parts[1])))

	resign := func(mutate func(header, claims map[string]any)) string {
		h, c := decodePart(t, parts[0]), decodePart(t, parts[1])
		mutate(h, c)
		hb, _ := json.Marshal(h)
		cb, _ := json.Marshal(c)
		signing := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
		return signing + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(signing)))
	}

	cases := map[string]struct {
		raw    string
		expect WorkloadAssertionExpectation
		code   string
		reason string
	}{
		"empty expected thumbprint": {
			raw:    fresh,
			expect: withWorkloadExpect(expect, func(e *WorkloadAssertionExpectation) { e.ExpectedKeyThumbprint = "" }),
			code:   CodeProofInvalid, reason: ReasonWorkloadAssertionKeyBinding,
		},
		"crit header rejected": {
			raw:    resign(func(h, _ map[string]any) { h["crit"] = []string{"jkt"} }),
			expect: expect,
			code:   CodeProofInvalid, reason: ReasonWorkloadAssertionHeader,
		},
		"alg none rejected": {
			raw:    resign(func(h, _ map[string]any) { h["alg"] = "none" }),
			expect: expect,
			code:   CodeProofInvalid, reason: ReasonWorkloadAssertionHeader,
		},
		"int64 lifetime overflow": {
			raw: resign(func(_, c map[string]any) {
				iat := now.Unix()
				c["iat"] = iat
				c["nbf"] = iat
				c["exp"] = iat + 300 + (int64(1) << 55)
			}),
			expect: expect,
			code:   CodeProofInvalid, reason: ReasonWorkloadAssertionLifetime,
		},
		"expired": {
			raw: fresh,
			expect: withWorkloadExpect(expect, func(e *WorkloadAssertionExpectation) {
				e.Now = now.Add(5*time.Minute + WorkloadClockSkew + time.Second)
			}),
			code: CodeTokenExpired, reason: ReasonWorkloadAssertionExpired,
		},
		"future": {
			raw:    fresh,
			expect: withWorkloadExpect(expect, func(e *WorkloadAssertionExpectation) { e.Now = now.Add(-WorkloadClockSkew - time.Second) }),
			code:   CodeProofInvalid, reason: ReasonWorkloadAssertionFuture,
		},
		"wrong workload": {
			raw:    fresh,
			expect: withWorkloadExpect(expect, func(e *WorkloadAssertionExpectation) { e.WorkloadID = "workload:other:api" }),
			code:   CodeProofInvalid, reason: ReasonWorkloadAssertionIdentity,
		},
		"wrong audience": {
			raw:    fresh,
			expect: withWorkloadExpect(expect, func(e *WorkloadAssertionExpectation) { e.Audience = "https://other.origin.internal" }),
			code:   CodeProofInvalid, reason: ReasonWorkloadAssertionAudience,
		},
		"wrong scope": {
			raw:    fresh,
			expect: withWorkloadExpect(expect, func(e *WorkloadAssertionExpectation) { e.Scope = "origin:admin" }),
			code:   CodeProofInvalid, reason: ReasonWorkloadAssertionScope,
		},
		"ineligible": {
			raw:    fresh,
			expect: withWorkloadExpect(expect, func(e *WorkloadAssertionExpectation) { e.Eligible = false }),
			code:   CodeIdentityRevoked, reason: ReasonWorkloadAssertionNotEligible,
		},
		"revoked key": {
			raw:    fresh,
			expect: withWorkloadExpect(expect, func(e *WorkloadAssertionExpectation) { e.RevokedThumbprints = []string{thumb} }),
			code:   CodeIdentityRevoked, reason: ReasonWorkloadAssertionRevoked,
		},
		"key binding mismatch": {
			raw:    fresh,
			expect: withWorkloadExpect(expect, func(e *WorkloadAssertionExpectation) { e.ExpectedKeyThumbprint = otherThumb }),
			code:   CodeProofInvalid, reason: ReasonWorkloadAssertionKeyBinding,
		},
		"forged signature": {
			raw:    forged,
			expect: expect,
			code:   CodeProofInvalid, reason: ReasonWorkloadAssertionSignature,
		},
	}
	for name, tc := range cases {
		if _, err := VerifyWorkloadAssertion(tc.raw, tc.expect, NewWorkloadReplayCache()); err == nil {
			t.Errorf("%s: accepted unexpectedly", name)
		} else if denied, ok := err.(*Error); !ok || denied.Code != tc.code || denied.Reason != tc.reason {
			t.Errorf("%s: got (%v), want (%s, %s)", name, err, tc.code, tc.reason)
		}
	}
}

func TestWorkloadQualificationEvidenceRoundTripAndVerdict(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	// Exact canonical JSON shape emitted by localevidence.ObserveWorkloadRelationship.
	canonicalWire := func(renewalState string, eligible bool, issuerStatus, clockStatus string) []byte {
		payload := map[string]any{
			"schema_version":                 WorkloadEvidenceSchema,
			"authority_version":              "stackkit.owner-workload-peer/v1",
			"guard_version":                  "0.52.4",
			"profile":                        map[string]string{"os": "linux", "init": "systemd", "transport": "mtls-loopback"},
			"peer_ref":                       "cloud-photos",
			"workload_id":                    "workload:cloud-photos:api",
			"service_ref":                    "photos",
			"edge_site_ref":                  "edge",
			"audience":                       "urn:stackkit:workload:photos",
			"scope":                          "origin:probe",
			"server_name":                    "photos.origin.stackkit.internal",
			"eligible":                       eligible,
			"eligibility_revision":           2,
			"eligibility_reason":             "approved",
			"ttl_seconds":                    300,
			"renew_before_seconds":           60,
			"max_session_seconds":            30,
			"active_certificate_digest":      "sha256:" + strings.Repeat("a", 64),
			"renewal_state":                  renewalState,
			"renewal_count":                  2,
			"retired_certificate_count":      1,
			"revoked_key_count":              0,
			"issuer_status":                  issuerStatus,
			"clock_status":                   clockStatus,
			"recovery_state":                 "none",
			"post_expiry_denied":             true,
			"measured_session_bound_seconds": 30,
			"observed_at":                    now.Format(time.RFC3339),
			"expires_at":                     now.Add(5 * time.Minute).Format(time.RFC3339),
			"evidence_age_seconds":           5,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	parsed, err := ParseWorkloadQualificationEvidence(canonicalWire("healthy", true, "available", "synchronized"), now.Add(10*time.Second))
	if err != nil {
		t.Fatalf("canonical wire parse failed: %v", err)
	}
	if verdict, reason := parsed.Judge(now.Add(10 * time.Second)); verdict != WorkloadVerdictQualified || reason != "" {
		t.Fatalf("verdict = (%q, %q), want qualified", verdict, reason)
	}
	if verdict, reason := parsed.Judge(now.Add(6 * time.Minute)); verdict != WorkloadVerdictUnknown || reason != "evidence_stale" {
		t.Fatalf("stale verdict = (%q, %q), want unknown/evidence_stale", verdict, reason)
	}

	// Revoked or expired renewal_state can NEVER be judged qualified even when eligible=true.
	for _, tc := range []struct {
		renewalState, issuerStatus, clockStatus string
		eligible                                bool
		wantVerdict, wantReason                 string
	}{
		{"revoked", "available", "synchronized", true, WorkloadVerdictUnqualified, "revoked"},
		{"expired", "available", "synchronized", true, WorkloadVerdictUnqualified, "expired"},
		{"renewal_stopped", "available", "synchronized", false, WorkloadVerdictUnqualified, "eligibility_removed"},
		{"issuer_outage", "outage", "synchronized", true, WorkloadVerdictDegraded, "issuer_outage"},
		{"clock_outage", "available", "skewed", true, WorkloadVerdictUnknown, "clock_outage"},
	} {
		ev, err := ParseWorkloadQualificationEvidence(canonicalWire(tc.renewalState, tc.eligible, tc.issuerStatus, tc.clockStatus), now.Add(10*time.Second))
		if err != nil {
			t.Fatalf("%s parse failed: %v", tc.renewalState, err)
		}
		if verdict, reason := ev.Judge(now.Add(10 * time.Second)); verdict != tc.wantVerdict || reason != tc.wantReason {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.renewalState, verdict, reason, tc.wantVerdict, tc.wantReason)
		}
	}

	if _, err := ParseWorkloadQualificationEvidence([]byte(`{"schema_version":"kombify.guard-workload-identity/v1","extra":"PRIVATE KEY"}`), now); err == nil {
		t.Fatal("evidence carrying unknown fields / PRIVATE KEY marker was accepted")
	}
}

func withWorkloadExpect(base WorkloadAssertionExpectation, fn func(*WorkloadAssertionExpectation)) WorkloadAssertionExpectation {
	fn(&base)
	return base
}
