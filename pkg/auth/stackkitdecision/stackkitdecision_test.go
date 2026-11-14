package stackkitdecision

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// The producer must build the bytes the StackKits consumer recomputes. These
// are the consumer's own vectors (internal/crypto/decisionreceipt,
// TestIntentDigestMatchesThePlatformVectors), pinned here byte for byte.
func TestIntentDigestMatchesTheConsumerVectors(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, vector := range []struct {
		name  string
		facts Facts
		want  string
	}{
		{"gateway example", Facts{Operation: OperationBackupRestore, Resource: "volume:srv-1/stack-media/data", ArgumentsSHA256: a, PlanSHA256: b, Revision: "card-rev-7",
			Principal: Principal{Kind: PrincipalAgentInstance, ID: "ainst-kubi"}}, "sha256:f7dd9caf241e837eb22520750148f1a3a47ca7a89d1eac6d0a45eaa5d7bbbff4"},
		{"contract restore", Facts{Operation: OperationBackupRestore, Resource: "stackkit:inst_0001:backup:bk_0042", ArgumentsSHA256: a, Revision: "rev_0007",
			Principal: Principal{Kind: PrincipalPerson, ID: "auth0|owner-0001"}}, "sha256:dc060cabbf8c591fd3152c78f8d4f580ae43a79532e03ac9282cd7fe13ed255e"},
		{"contract change set", Facts{Operation: OperationAdvancedChangeSetApply, Resource: "stackkit:inst_0001:change-set:cs_0003", ArgumentsSHA256: a, PlanSHA256: b, Revision: "rev_0007",
			Principal: Principal{Kind: PrincipalAgentInstance, ID: "ainst_homelab_ops"}}, "sha256:b09d2c498919c2c930184f411c725eb3274c265ba1901dd6215c574a75df567a"},
		{"contract installation", Facts{Operation: OperationInstallationRemove, Resource: "stackkit:inst_0001", ArgumentsSHA256: a, Revision: "rev_0007",
			Principal: Principal{Kind: PrincipalAgentInstance, ID: "ainst_homelab_ops"}}, "sha256:d58ebb6b30fa06b830b6a7b26530e709ebaac0c3c17baff33b06f1c6f200adff"},
	} {
		got, err := IntentDigest(vector.facts)
		if err != nil || got != vector.want {
			t.Fatalf("%s: got %s (%v), want %s", vector.name, got, err, vector.want)
		}
	}
}

func TestArgumentsDigestIsTheConsumerDefinition(t *testing.T) {
	got, err := ArgumentsDigest(BackupRestoreArguments("sha256:"+strings.Repeat("c", 64), "", "", "restore-0001"))
	if err != nil {
		t.Fatal(err)
	}
	canonical := `{"marketplace_service":"","operation_id":"restore-0001","restore_result_id":"sha256:` + strings.Repeat("c", 64) + `","workload":""}`
	sum := sha256.Sum256([]byte(ArgumentsDigestDomain + "\n" + canonical + "\n"))
	if got != hex.EncodeToString(sum[:]) {
		t.Fatalf("restore arguments digest %s does not follow the definition", got)
	}
	got, err = ArgumentsDigest(AdvancedChangeSetApplyArguments("sha256:" + strings.Repeat("e", 64)))
	if err != nil {
		t.Fatal(err)
	}
	sum = sha256.Sum256([]byte(ArgumentsDigestDomain + "\n" + `{"change_set_id":"sha256:` + strings.Repeat("e", 64) + `"}` + "\n"))
	if got != hex.EncodeToString(sum[:]) {
		t.Fatalf("change-set arguments digest %s does not follow the definition", got)
	}
}

func TestCanonicalStringsFollowJSONStringify(t *testing.T) {
	got, err := canonicalJSON(map[string]any{"b": " \b\f\x01", "a": json.Number("7"), "\U0001F600": true, "￿": false})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"a\":7,\"b\":\" \\b\\f\\u0001\",\"\U0001F600\":true,\"￿\":false}"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTargetReferenceCarriesTheServer(t *testing.T) {
	for _, vector := range []struct{ kind, ref, want string }{
		{"", "", "stackkit:srv-0001"},
		{TargetRestore, "sha256:" + strings.Repeat("c", 64), "stackkit:srv-0001:restore:sha256:" + strings.Repeat("c", 64)},
		{TargetChangeSet, "sha256:" + strings.Repeat("a", 64), "stackkit:srv-0001:change-set:sha256:" + strings.Repeat("a", 64)},
	} {
		got, err := Reference("srv-0001", vector.kind, vector.ref)
		if err != nil || got != vector.want {
			t.Fatalf("reference %q/%q: got %q (%v)", vector.kind, vector.ref, got, err)
		}
	}
	if _, err := Reference("", TargetRestore, "x"); err == nil {
		t.Fatal("a target without the node's server id must be refused")
	}
}

func testReceipt(t *testing.T, mutate func(map[string]any)) json.RawMessage {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	receipt := map[string]any{
		"receiptId": "rcpt-0001", "decisionId": "dec-0001", "operationId": "op-0001", "subjectStore": SubjectStore,
		"issuedAt": now.Add(-time.Minute).Format(time.RFC3339), "expiresAt": now.Add(10 * time.Minute).Format(time.RFC3339),
		"outcome": "allowed", "optionKind": "allow_once", "answeredBy": map[string]any{"kind": "person", "id": "auth0|owner-0001"},
		"surface": "companion", "assurance": "L1", "intentDigest": "sha256:" + strings.Repeat("f", 64),
		"lifetime":  map[string]any{"scope": "once"},
		"signature": map[string]any{"alg": "ES256", "kid": "dr-1", "value": "c2lnbmF0dXJl"},
	}
	if mutate != nil {
		mutate(receipt)
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func expectRefusal(t *testing.T, err error, name string) {
	t.Helper()
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Name != name {
		t.Fatalf("expected %s, got %v", name, err)
	}
}

func TestCheckAdmitsOnlyTheFiledAllowedReceipt(t *testing.T) {
	digest := "sha256:" + strings.Repeat("f", 64)
	now := time.Now().UTC()
	admitted, err := Check(testReceipt(t, nil), "dec-0001", digest, now)
	if err != nil {
		t.Fatalf("the filed allowed receipt must be admitted: %v", err)
	}
	parts := strings.Split(admitted.Compact, ".")
	if len(parts) != 3 || parts[2] != "c2lnbmF0dXJl" {
		t.Fatalf("compact form %q", admitted.Compact)
	}
	header, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if string(header) != `{"alg":"ES256","kid":"dr-1","typ":"kombify.decision-receipt.v1"}` {
		t.Fatalf("header %s", header)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if strings.Contains(string(payload), "signature") || !strings.HasPrefix(string(payload), `{"answeredBy":{"id":"auth0|owner-0001","kind":"person"},"assurance":"L1",`) {
		t.Fatalf("payload %s", payload)
	}

	expectRefusal(t, func() error {
		_, err := Check(testReceipt(t, func(r map[string]any) { r["outcome"] = "denied" }), "dec-0001", digest, now)
		return err
	}(), RefusalInvalid)
	expectRefusal(t, func() error {
		_, err := Check(testReceipt(t, func(r map[string]any) { r["subjectStore"] = "ril_action" }), "dec-0001", digest, now)
		return err
	}(), RefusalInvalid)
	expectRefusal(t, func() error {
		_, err := Check(testReceipt(t, func(r map[string]any) { r["surface"] = "equipment" }), "dec-0001", digest, now)
		return err
	}(), RefusalInvalid)
	expectRefusal(t, func() error { _, err := Check(testReceipt(t, nil), "dec-0002", digest, now); return err }(), RefusalTargetMismatch)
	expectRefusal(t, func() error {
		_, err := Check(testReceipt(t, nil), "dec-0001", "sha256:"+strings.Repeat("0", 64), now)
		return err
	}(), RefusalTargetMismatch)
	expectRefusal(t, func() error {
		_, err := Check(testReceipt(t, nil), "dec-0001", digest, now.Add(20*time.Minute))
		return err
	}(), RefusalExpired)
	expectRefusal(t, func() error {
		_, err := Check(testReceipt(t, func(r map[string]any) {
			r["expiresAt"] = time.Now().UTC().Add(30 * time.Minute).Format(time.RFC3339)
		}), "dec-0001", digest, now)
		return err
	}(), RefusalInvalid)
}
