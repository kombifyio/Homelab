//go:build !guardqualification

package guardrelease

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
)

// A default build verifies only the release workflow. A certificate of a
// qualification-workflow run, signed by the same public-good Sigstore and
// otherwise well formed, is refused: no production Guard trusts a harness run.
func TestDefaultBuildRefusesAQualificationRunSigner(t *testing.T) {
	pinned, err := pinnedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := newCertificateIdentity(pinned)
	if err != nil {
		t.Fatal(err)
	}
	cert := func(workflow, ref string) certificate.Summary {
		signer := "https://github.com/kombifyio/TechStack/.github/workflows/" + workflow + "@" + ref
		return certificate.Summary{
			SubjectAlternativeName: signer,
			Extensions: certificate.Extensions{
				Issuer: GitHubOIDCIssuer, BuildSignerURI: signer, BuildTrigger: "workflow_dispatch", RunnerEnvironment: "github-hosted",
				SourceRepositoryURI: releaseRepositoryURI, SourceRepositoryOwnerURI: "https://github.com/kombifyio",
				SourceRepositoryRef: ref, SourceRepositoryDigest: "0123456789abcdef0123456789abcdef01234567",
			},
		}
	}
	if err := identity.Verify(cert("guard-release.yml", "refs/heads/main")); err != nil {
		t.Fatalf("the release workflow was refused: %v", err)
	}
	if err := identity.Verify(cert("guard-qualification.yml", "refs/heads/main")); err == nil {
		t.Fatal("a guard-qualification run was admitted by a default build")
	}
}

func TestNativeBuildBindsParentSlotAndFourSubjects(t *testing.T) {
	b := &BuildProvenance{Schema: "guard.native-build/v1", SourceSHA: strings.Repeat("a", 40), SourceTree: strings.Repeat("b", 40),
		ExecutorSHA: strings.Repeat("c", 40), ExecutorWorkflow: "kombifyio/TechStack/.github/workflows/guard-release.yml@refs/heads/main",
		ParentRunID: 10, ParentAttempt: 1, PlanDigest: "sha256:" + strings.Repeat("d", 64), PlanArtifactID: 123, PlanArchiveDigest: "sha256:" + strings.Repeat("e", 64),
		PlanArtifactName: "delivery-plan-10-1", CompilerRef: strings.Repeat("f", 40), ChildRunID: 20, ChildAttempt: 1,
		BindingName: fmt.Sprintf("delivery-dispatch-10-1-%x", sha256.Sum256([]byte(`["techstack-service","runtime-service","publish",0]`))), BindingDigest: "sha256:" + strings.Repeat("1", 64), Subjects: map[string]BuildSubject{}}
	for _, name := range []string{"techstack-linux-amd64", "techstack-linux-arm64", "stackkit-runtime-linux-amd64.tar.gz", "stackkit-runtime-linux-arm64.tar.gz"} {
		b.Subjects[name] = BuildSubject{SHA256: strings.Repeat("2", 64), Size: 123}
	}
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*BuildProvenance){func(b *BuildProvenance) { b.ParentRunID++ }, func(b *BuildProvenance) { b.ParentAttempt++ },
		func(b *BuildProvenance) { b.BindingName = "delivery-dispatch-10-1-" + strings.Repeat("3", 64) }, func(b *BuildProvenance) { b.ExecutorWorkflow = "other" },
		func(b *BuildProvenance) { b.ChildAttempt = 2 }} {
		altered := *b
		mutate(&altered)
		if err := altered.Validate(); err == nil {
			t.Fatal("changed native build authority was accepted")
		}
	}
	delete(b.Subjects, "techstack-linux-amd64")
	b.Subjects["foreign-file"] = BuildSubject{SHA256: strings.Repeat("2", 64), Size: 123}
	if err := b.Validate(); err == nil {
		t.Fatal("substituted build subject was accepted")
	}
}

func TestManifestRejectsAmbiguousSignedObjects(t *testing.T) {
	for _, raw := range []string{`{"buildProvenance":{"sourceSha":"a","sourceSha":"b"}}`, `{"artifacts":[],"artifacts":[]}`, strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66)} {
		if _, err := ParseManifest([]byte(raw)); err == nil {
			t.Fatal("ambiguous or unbounded signed object was accepted")
		}
	}
	if err := uniqueManifestKeys([]byte(`{"subjects":{"file":{"size":1}},"list":[{"x":true}]}`)); err != nil {
		t.Fatal(err)
	}
}
