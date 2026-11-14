package stackkitrelease

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyCapabilityBoundToBinaryDigest(t *testing.T) {
	old := strings.NewReader("old release without capability declaration")
	before, err := digestArchiveReader(old, int64(old.Len()))
	if err != nil || before.verifyExpectedPlan {
		t.Fatalf("old binary admitted: %v", err)
	}
	binary := "prefix" + verifyPlanManifest + "suffix"
	input := strings.NewReader(binary)
	actual, err := digestArchiveReader(input, int64(input.Len()))
	if err != nil || !actual.verifyExpectedPlan || actual.sha256 == before.sha256 {
		t.Fatalf("new binary not bound to digest: %v", err)
	}
	scanner := &capabilityScanner{}
	marker := []byte(verifyPlanManifest)
	_, _ = scanner.Write(marker[:11])
	_, _ = scanner.Write(marker[11:])
	if scanner.matches != 1 {
		t.Fatal("declaration split across reads was missed")
	}
	duplicate := strings.NewReader(binary + binary)
	duplicateDigest, err := digestArchiveReader(duplicate, int64(duplicate.Len()))
	if err != nil || duplicateDigest.verifyExpectedPlan {
		t.Fatal("ambiguous duplicate declaration admitted")
	}
	path := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(path, []byte(binary), 0600); err != nil {
		t.Fatal(err)
	}
	admitted, err := verifyCapabilityBinary(path, actual.sha256)
	if err != nil || !admitted {
		t.Fatalf("matching binary refused: %v", err)
	}
	if _, err := verifyCapabilityBinary(path, before.sha256); err == nil {
		t.Fatal("binary admitted against a different signed digest")
	}
	missing, err := verifyCapabilityBinary(path+"-missing", actual.sha256)
	if err != nil || missing {
		t.Fatal("missing legacy binary granted capability")
	}
}

func TestRILPrimitiveCapabilityRequiresUniqueDigestBoundMarkers(t *testing.T) {
	const verifyHash = "sha256:8d38857c71ceec1b2558aa842dd04f31ec116e3dd0e7dd48eec3e592bccefabe"
	const restartHash = "sha256:2867f83c897822c1eacb127cfe850ab5f8d610bf38078aa0dab2ad4105634693"
	marker := func(id, hash string) string {
		return "stackkit.ril-primitive-contract/v2:" + id + ":" + hash + ":end-stackkit.ril-primitive-contract"
	}
	scanner := &capabilityScanner{}
	raw := []byte(rilPrimitiveManifest + marker("verify-stackkit-state", verifyHash) + "\x00" + marker("restart-service", restartHash) + marker("apply-stackkit-change", verifyHash))
	for _, b := range raw {
		_, _ = scanner.Write([]byte{b})
	}
	release := Release{rilPrimitiveHashes: scanner.qualifiedRILHashes()}
	if release.RILPrimitiveHash("verify-stackkit-state") != verifyHash || release.RILPrimitiveHash("restart-service") != restartHash || !release.RILPrimitiveSupported("restart-service") {
		t.Fatal("split signed per-primitive capability was not retained")
	}
	if release.RILPrimitiveSupported("update-service") || release.RILPrimitiveSupported("apply-stackkit-change") {
		t.Fatal("a primitive without its own executor-bound marker became supported")
	}
	_, _ = scanner.Write([]byte(marker("restart-service", restartHash)))
	if scanner.qualifiedRILHashes() != nil {
		t.Fatal("duplicate primitive authority was admitted")
	}
	old := strings.NewReader(verifyPlanManifest + "stackkit.ril-primitive-contract/v1:" + verifyHash + ":end-stackkit.ril-primitive-contract")
	digest, err := digestArchiveReader(old, int64(old.Len()))
	if err != nil || digest.rilPrimitiveHashes != nil {
		t.Fatal("old binary gained RIL primitive capability")
	}
}
