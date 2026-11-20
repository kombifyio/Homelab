package guardrelease

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// BuildProvenance is signed inside the immutable publication seed. Fulcio
// authenticates ExecutorSHA; this statement binds that executor's admitted
// native child, exact checked-out source tree and the immutable output bytes.
type BuildProvenance struct {
	Schema            string                  `json:"schema"`
	SourceSHA         string                  `json:"sourceSha"`
	SourceTree        string                  `json:"sourceTree"`
	ExecutorSHA       string                  `json:"executorSha"`
	ExecutorWorkflow  string                  `json:"executorWorkflow"`
	ParentRunID       int64                   `json:"parentRunId"`
	ParentAttempt     int                     `json:"parentAttempt"`
	PlanDigest        string                  `json:"planDigest"`
	PlanArtifactID    int64                   `json:"planArtifactId"`
	PlanArchiveDigest string                  `json:"planArchiveDigest"`
	PlanArtifactName  string                  `json:"planArtifactName"`
	CompilerRef       string                  `json:"compilerRef"`
	ChildRunID        int64                   `json:"childRunId"`
	ChildAttempt      int                     `json:"childAttempt"`
	BindingName       string                  `json:"bindingName"`
	BindingDigest     string                  `json:"bindingDigest"`
	Subjects          map[string]BuildSubject `json:"subjects"`
}
type BuildSubject struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

var commitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var planDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func (b *BuildProvenance) Validate() error {
	if b == nil {
		return errors.New("signed native build provenance is missing")
	}
	if b.Schema != "guard.native-build/v1" || !commitPattern.MatchString(b.SourceSHA) || !commitPattern.MatchString(b.SourceTree) || !commitPattern.MatchString(b.ExecutorSHA) || !commitPattern.MatchString(b.CompilerRef) ||
		b.ExecutorWorkflow != strings.TrimPrefix(releaseRepositoryURI, "https://github.com/")+"/.github/workflows/guard-release.yml@refs/heads/main" || b.ParentRunID <= 0 || b.ParentAttempt <= 0 || b.ChildRunID <= 0 || b.ChildAttempt != 1 ||
		!planDigestPattern.MatchString(b.PlanDigest) || b.PlanArtifactID <= 0 || !planDigestPattern.MatchString(b.PlanArchiveDigest) || b.PlanArtifactName != fmt.Sprintf("delivery-plan-%d-%d", b.ParentRunID, b.ParentAttempt) ||
		b.BindingName != fmt.Sprintf("delivery-dispatch-%d-%d-%x", b.ParentRunID, b.ParentAttempt, sha256.Sum256([]byte(`["homelab-service","runtime-service","publish",0]`))) || !planDigestPattern.MatchString(b.BindingDigest) || len(b.Subjects) != 4 {
		return errors.New("signed native build provenance is invalid")
	}
	for _, name := range []string{"homelab-linux-amd64", "homelab-linux-arm64", "stackkit-runtime-linux-amd64.tar.gz", "stackkit-runtime-linux-arm64.tar.gz"} {
		subject, ok := b.Subjects[name]
		if !ok || !digestPattern.MatchString(subject.SHA256) || subject.Size <= 0 {
			return errors.New("signed native build subject is missing or invalid")
		}
	}
	return nil
}
