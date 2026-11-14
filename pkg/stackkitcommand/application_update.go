package stackkitcommand

import (
	"fmt"
	"regexp"

	"github.com/kombifyio/techstack/pkg/api/agentpb"
)

// ApplicationUpdateOperation is the catalog operation of the single-application
// update on a Techstack-managed node (StackKits #3262): `stackkit use-cases
// upgrade <slug> --to <tag> --digest sha256:... --capability <file> --apply
// --owner-approve --json`. Techstack advertises and dispatches the update only
// when the pinned release's catalog lists it as dispatchable; the node admits
// it only under the exact application-release.update capability of the
// approved use case, release, digest and tested or untested kind.
const ApplicationUpdateOperation = "application-release.update"

// ApplicationUpdateResultAPIVersion names the CLI's `--apply --json` result.
const ApplicationUpdateResultAPIVersion = "stackkit.application-update-result/v1"

var (
	applicationUpdateUseCasePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// An OCI tag: the CLI resolves it inside the module's release track.
	applicationUpdateVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	applicationUpdateDigestPattern  = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// ValidApplicationUpdateTarget reports whether a use case slug, release tag
// and manifest digest are admissible CLI inputs (and capability scope fields).
func ValidApplicationUpdateTarget(useCase, version, digest string) bool {
	return applicationUpdateUseCasePattern.MatchString(useCase) && applicationUpdateVersionPattern.MatchString(version) &&
		applicationUpdateDigestPattern.MatchString(digest)
}

// ApplicationUpdateReleaseKind is the capability scope's releaseKind.
func ApplicationUpdateReleaseKind(acceptUntested bool) string {
	if acceptUntested {
		return "untested"
	}
	return "tested"
}

func applicationUpdateFieldsPresent(command *agentpb.StackKitCommand) bool {
	return command.UseCase != "" || command.ApplicationVersion != "" || command.ApplicationDigest != "" || command.AcceptUntested
}

// validateApplicationUpdateFields keeps the update inputs on USE_CASE_UPGRADE.
func validateApplicationUpdateFields(command *agentpb.StackKitCommand) error {
	if command.Operation != agentpb.StackKitOperation_STACKKIT_OPERATION_USE_CASE_UPGRADE && applicationUpdateFieldsPresent(command) {
		return fmt.Errorf("StackKit application update fields are only valid for a use case upgrade")
	}
	return nil
}

func validateApplicationUpdateCommand(command *agentpb.StackKitCommand) error {
	if command.Release == nil {
		return fmt.Errorf("StackKit application update requires its pinned StackKits release")
	}
	if !command.OwnerApproved {
		return fmt.Errorf("StackKit application update requires explicit Owner approval")
	}
	if len(command.AdvancedCapability) == 0 || len(command.AdvancedCapability) > MaxAdvancedCapabilityBytes {
		return fmt.Errorf("StackKit application update requires its application-release.update capability within %d bytes", MaxAdvancedCapabilityBytes)
	}
	if command.DryRun || command.Offline || command.TargetRelease != "" || len(command.InventoryJson) != 0 || len(command.CandidateSpecJson) != 0 ||
		command.RilExecutionProof != nil || command.ServiceKey != "" || command.ExpectedPlanHash != "" {
		return fmt.Errorf("StackKit application update takes only its use case, release, digest, acknowledgement and capability")
	}
	if !ValidApplicationUpdateTarget(command.UseCase, command.ApplicationVersion, command.ApplicationDigest) {
		return fmt.Errorf("StackKit application update needs a use case slug, a release tag and a sha256 digest")
	}
	return nil
}

// ApplicationUpdateArgValues are the catalog placeholder values of one
// update; the Guard adds the private capability file it wrote.
func ApplicationUpdateArgValues(command *agentpb.StackKitCommand, capabilityFile string) (map[string]string, error) {
	if command == nil || command.Operation != agentpb.StackKitOperation_STACKKIT_OPERATION_USE_CASE_UPGRADE {
		return nil, fmt.Errorf("StackKit application update command is required")
	}
	if err := validateApplicationUpdateCommand(command); err != nil {
		return nil, err
	}
	return map[string]string{
		"useCase": command.UseCase, "releaseTag": command.ApplicationVersion, "imageDigest": command.ApplicationDigest, "capabilityFile": capabilityFile,
	}, nil
}
