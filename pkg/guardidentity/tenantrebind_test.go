package guardidentity

import (
	"errors"
	"testing"
	"time"
)

func TestValidateTenantRebindPairOnlyMovesOneSubjectToItsCanonicalTenant(t *testing.T) {
	// Synthetic Auth0-shaped subject: the guard distinguishes subject identity, not the numeric id.
	const subject = "google-oauth2|1"
	cases := []struct {
		name, from, to, subject, label string
		ok                             bool
	}{
		{"bare subject", subject, "usr:" + subject, subject, "", true},
		{"mapped label", "marcel-personal", "usr:" + subject, subject, "marcel-personal", true},
		{"unmapped label", "marcel-personal", "usr:" + subject, subject, "", false},
		{"label mapped to another value", "marcel-personal", "usr:" + subject, subject, "other", false},
		{"other subject target", subject, "usr:auth0|someone-else", subject, "", false},
		{"canonical source", "usr:" + subject, "usr:" + subject, subject, "", false},
		{"usr source label", "usr:auth0|x", "usr:" + subject, subject, "usr:auth0|x", false},
		{"subject with space", "a b", "usr:a b", "a b", "", false},
		{"empty subject", "", "usr:", "", "", false},
	}
	for _, tc := range cases {
		err := ValidateTenantRebindPair(tc.from, tc.to, tc.subject, tc.label)
		if (err == nil) != tc.ok {
			t.Fatalf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrTenantRebindRefused) {
			t.Fatalf("%s: err = %v, want ErrTenantRebindRefused", tc.name, err)
		}
	}
}

func validRebindOffer(t *testing.T, now time.Time) TenantRebindOffer {
	t.Helper()
	id, err := NewTenantRebindDirectiveID()
	if err != nil {
		t.Fatal(err)
	}
	return TenantRebindOffer{
		Directive: TenantRebindDirective{
			ContractVersion: TenantRebindContractVersion, DirectiveID: id,
			FromTenantID: "auth0|demo", ToTenantID: "usr:auth0|demo", SubjectID: "auth0|demo",
			RuntimeAgentID: "worker_1", ServerID: "server_1", CredentialGeneration: 1,
			IssuedAt: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		},
		AgentToken: "tsra.opaque.abc",
	}
}

func TestTenantRebindOfferBindsTheGuardsCurrentTenantAndWorker(t *testing.T) {
	now := time.Now().UTC()
	offer := validRebindOffer(t, now)
	if err := offer.ValidateForGuard("auth0|demo", "worker_1", now); err != nil {
		t.Fatalf("valid offer refused: %v", err)
	}
	for name, check := range map[string]func() error{
		"other tenant": func() error { return offer.ValidateForGuard("marcel-personal", "worker_1", now) },
		"other worker": func() error { return offer.ValidateForGuard("auth0|demo", "worker_2", now) },
		"expired":      func() error { return offer.ValidateForGuard("auth0|demo", "worker_1", now.Add(2*time.Hour)) },
		"signed token": func() error {
			copy := offer
			copy.AgentToken = "tsra.v1.payload.sig"
			return copy.ValidateForGuard("auth0|demo", "worker_1", now)
		},
		"target of another subject": func() error {
			copy := offer
			copy.Directive.ToTenantID = "usr:auth0|other"
			return copy.ValidateForGuard("auth0|demo", "worker_1", now)
		},
		"lifetime beyond bound": func() error {
			copy := offer
			copy.Directive.NotAfter = copy.Directive.IssuedAt.Add(TenantRebindMaxLifetime + time.Second)
			return copy.ValidateForGuard("auth0|demo", "worker_1", now)
		},
	} {
		if err := check(); !errors.Is(err, ErrTenantRebindRefused) {
			t.Fatalf("%s: err = %v, want ErrTenantRebindRefused", name, err)
		}
	}
}

func TestTenantRebindDigestIgnoresAcknowledgementAndBindsIdentity(t *testing.T) {
	now := time.Now().UTC()
	directive := validRebindOffer(t, now).Directive
	digest := directive.Digest()
	acknowledged := directive
	acknowledged.StagedAt = &now
	if acknowledged.Digest() != digest {
		t.Fatal("the acknowledgement changed the directive digest")
	}
	moved := directive
	moved.ToTenantID = "usr:auth0|other"
	if moved.Digest() == digest {
		t.Fatal("the digest does not bind the target tenant")
	}
	value, err := directive.ResourceValue()
	if err != nil {
		t.Fatal(err)
	}
	decoded, ok := DirectiveFromResource(value)
	if !ok || decoded.Digest() != digest {
		t.Fatal("the resource encoding does not round-trip")
	}
}
