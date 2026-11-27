package stackkitcommand

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/kombifyio/homelab/pkg/api/agentpb"
	"strings"
	"testing"
)

func TestVerifyPlanEvidenceBindsAppliedPlan(t *testing.T) {
	hash := "sha256:" + strings.Repeat("a", 64)
	other := "sha256:" + strings.Repeat("b", 64)
	pin := &agentpb.StackKitReleasePin{Version: "v0.55.5", PlatformOs: "linux", PlatformArch: "amd64", ArchiveSha256: strings.Repeat("a", 64), ReleaseIndexSha256: strings.Repeat("b", 64)}
	cmd := &agentpb.StackKitCommand{Release: pin, CommandId: "exec-001", Operation: agentpb.StackKitOperation_STACKKIT_OPERATION_VERIFY, ExpectedPlanHash: hash, Offline: true}
	makeResult := func(plan, applied, resultHash string, offline string) *agentpb.StackKitResult {
		raw := []byte(fmt.Sprintf(`{"schemaVersion":"stackkit.command-result/v1","command":"stackkit verify","status":"success","data":{"schemaVersion":"stackkit.verify-result/v1","offline":%s,"planHash":%q,"apply":{"planHash":%q,"resultHash":%q,"evidenceBundleHash":%q}}}`, offline, plan, applied, resultHash, hash))
		sum := sha256.Sum256(raw)
		return &agentpb.StackKitResult{CommandId: cmd.CommandId, Release: pin, Success: true, CommandResultJson: raw, PinnedCliEvidenceVerified: true, PinnedCliEvidenceSha256: "sha256:" + hex.EncodeToString(sum[:])}
	}
	good := makeResult(hash, hash, hash, "true")
	if _, err := ParseVerifyPlanEvidence(cmd, good); err != nil {
		t.Fatal(err)
	}
	cases := []*agentpb.StackKitResult{makeResult(other, hash, hash, "true"), makeResult(hash, other, hash, "true"), makeResult(hash, hash, "", "true"), makeResult(hash, hash, hash, "false")}
	missing := makeResult(hash, hash, hash, "true")
	missing.PinnedCliEvidenceVerified = false
	cases = append(cases, missing)
	tampered := makeResult(hash, hash, hash, "true")
	tampered.CommandResultJson = append(tampered.CommandResultJson, ' ')
	cases = append(cases, tampered)
	wrongRelease := makeResult(hash, hash, hash, "true")
	wrongRelease.Release = &agentpb.StackKitReleasePin{Version: "v0.55.4", PlatformOs: pin.PlatformOs, PlatformArch: pin.PlatformArch, ArchiveSha256: pin.ArchiveSha256, ReleaseIndexSha256: pin.ReleaseIndexSha256}
	cases = append(cases, wrongRelease)
	badExit := makeResult(hash, hash, hash, "true")
	badExit.ExitCode = 1
	cases = append(cases, badExit)
	for _, result := range cases {
		if _, err := ParseVerifyPlanEvidence(cmd, result); err == nil {
			t.Fatal("accepted mismatched or unattested verify result")
		}
	}
}

func TestValidateRestoreActivationAdmitsCloudKitAndBasementKit(t *testing.T) {
	pin := &agentpb.StackKitReleasePin{
		Version:            "v0.61.11",
		PlatformOs:         "linux",
		PlatformArch:       "amd64",
		ArchiveSha256:      strings.Repeat("a", 64),
		ReleaseIndexSha256: strings.Repeat("b", 64),
	}
	digest := "sha256:" + strings.Repeat("c", 64)
	for _, tc := range []struct {
		name    string
		kit     string
		site    string
		node    string
		chanRef string
	}{
		{name: "cloud-kit", kit: "cloud-kit", site: "cloud", node: "cloud-main", chanRef: "host-channel-cloud-main"},
		{name: "basement-kit", kit: "basement-kit", site: "home", node: "main", chanRef: "local-home-main"},
	} {
		cmd := &agentpb.StackKitCommand{
			CommandId:                "sr-1-final-sync",
			Operation:                agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RESTORE_ACTIVATE,
			WorkingDirectory:         "/opt/stackkit",
			Release:                  pin,
			OwnerApproved:            true,
			Stackkit:                 tc.kit,
			StackkitInstanceId:       "stack-1",
			ExpectedPlanHash:         digest,
			RestoreResultId:          digest,
			ActivationOperationId:    "sr-1-activate",
			LocalSiteRef:             tc.site,
			LocalNodeRef:             tc.node,
			LocalExecutionChannelRef: tc.chanRef,
		}
		if err := ValidateCommand(cmd); err != nil {
			t.Fatalf("%s ValidateCommand failed: %v", tc.name, err)
		}
	}

	missingChannel := &agentpb.StackKitCommand{
		CommandId:             "sr-1-bad",
		Operation:             agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RESTORE_ACTIVATE,
		WorkingDirectory:      "/opt/stackkit",
		Release:               pin,
		OwnerApproved:         true,
		Stackkit:              "cloud-kit",
		StackkitInstanceId:    "stack-1",
		ExpectedPlanHash:      digest,
		RestoreResultId:       digest,
		ActivationOperationId: "sr-1-activate",
	}
	if err := ValidateCommand(missingChannel); err == nil {
		t.Fatal("expected ValidateCommand to reject cloud-kit restore activation without local placement")
	}

	cloudRecover := &agentpb.StackKitCommand{
		CommandId:                "sr-1-recover",
		Operation:                agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RESTORE_RECOVER,
		WorkingDirectory:         "/opt/stackkit",
		Release:                  pin,
		OwnerApproved:            true,
		Stackkit:                 "cloud-kit",
		StackkitInstanceId:       "stack-1",
		ExpectedPlanHash:         digest,
		RestoreResultId:          digest,
		ActivationOperationId:    "sr-1-activate",
		LocalSiteRef:             "cloud",
		LocalNodeRef:             "cloud-main",
		LocalExecutionChannelRef: "host-channel-cloud-main",
	}
	if err := ValidateCommand(cloudRecover); err == nil {
		t.Fatal("expected ValidateCommand to reject cloud-kit BACKUP_RESTORE_RECOVER")
	}

	oldCloudPin := &agentpb.StackKitReleasePin{
		Version:            "v0.61.10",
		PlatformOs:         "linux",
		PlatformArch:       "amd64",
		ArchiveSha256:      strings.Repeat("a", 64),
		ReleaseIndexSha256: strings.Repeat("b", 64),
	}
	oldCloudActivate := &agentpb.StackKitCommand{
		CommandId:                "sr-1-old-pin",
		Operation:                agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RESTORE_ACTIVATE,
		WorkingDirectory:         "/opt/stackkit",
		Release:                  oldCloudPin,
		OwnerApproved:            true,
		Stackkit:                 "cloud-kit",
		StackkitInstanceId:       "stack-1",
		ExpectedPlanHash:         digest,
		RestoreResultId:          digest,
		ActivationOperationId:    "sr-1-activate",
		LocalSiteRef:             "cloud",
		LocalNodeRef:             "cloud-main",
		LocalExecutionChannelRef: "host-channel-cloud-main",
	}
	if err := ValidateCommand(oldCloudActivate); err == nil {
		t.Fatal("expected ValidateCommand to reject cloud-kit BACKUP_RESTORE_ACTIVATE below MinCloudRestoreActivationRelease")
	}

	wrongCloudSite := &agentpb.StackKitCommand{
		CommandId:                "sr-1-wrong-site",
		Operation:                agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RESTORE_ACTIVATE,
		WorkingDirectory:         "/opt/stackkit",
		Release:                  pin,
		OwnerApproved:            true,
		Stackkit:                 "cloud-kit",
		StackkitInstanceId:       "stack-1",
		ExpectedPlanHash:         digest,
		RestoreResultId:          digest,
		ActivationOperationId:    "sr-1-activate",
		LocalSiteRef:             "home",
		LocalNodeRef:             "cloud-main",
		LocalExecutionChannelRef: "host-channel-cloud-main",
	}
	if err := ValidateCommand(wrongCloudSite); err == nil {
		t.Fatal("expected ValidateCommand to reject cloud-kit BACKUP_RESTORE_ACTIVATE outside cloud/cloud-main")
	}
}
