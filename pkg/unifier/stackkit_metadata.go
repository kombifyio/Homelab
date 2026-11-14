package unifier

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/kombifyio/techstack/internal/stackkitrelease"
)

func stackKitMetadataAuthority(explicit ...string) (string, error) {
	if len(explicit) > 0 && strings.TrimSpace(explicit[0]) != "" {
		return secureExistingDir(explicit[0])
	}
	for _, name := range []string{"TECHSTACK_STACKKITS_DIR", "STACKKITS_REPO", "STACKKITS_PATH"} {
		if directory := strings.TrimSpace(os.Getenv(name)); directory != "" {
			root, err := secureExistingDir(directory)
			if err != nil {
				return "", fmt.Errorf("configured StackKits metadata authority %s: %w", name, err)
			}
			return root, nil
		}
	}
	return "", nil
}

// ListKitInfo reads the released metadata authority. Both HTTP catalog routes
// use it, and broken overrides remain errors.
func (l *StackKitLoader) ListKitInfo() ([]*StackKitInfo, error) {
	if l.metadataError != nil {
		return nil, l.metadataError
	}
	metadata, err := stackkitrelease.ReadKitMetadata(l.metadataDir)
	if err != nil {
		return nil, err
	}
	result := make([]*StackKitInfo, 0, len(metadata))
	for _, kit := range metadata {
		result = append(result, stackKitInfoFromMetadata(kit))
	}
	return result, nil
}

func (l *StackKitLoader) GetKitInfo(name string) (*StackKitInfo, error) {
	if err := validateStackKitName(name); err != nil {
		return nil, fmt.Errorf("%w: %s", stackkitrelease.ErrKitMetadataNotFound, name)
	}
	kits, err := l.ListKitInfo()
	if err != nil {
		return nil, err
	}
	name = CanonicalStackKitName(name)
	for _, kit := range kits {
		if kit.Name == name {
			return kit, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", stackkitrelease.ErrKitMetadataNotFound, name)
}

// MultiNodeFeature is the kit metadata feature with which a StackKits release
// declares that a deployment of the kit takes additional nodes.
const MultiNodeFeature = "multiNode"

// ErrMultiNodeJoinUnsupported means the pinned release does not declare
// multi-node join for the kit, so no server may join its deployments.
var ErrMultiNodeJoinUnsupported = errors.New("the pinned StackKits release does not declare multi-node join for this StackKit")

// RequireMultiNodeJoin fails closed unless the released metadata of the kit
// declares MultiNodeFeature. An unknown or unreadable kit is refused too.
func (l *StackKitLoader) RequireMultiNodeJoin(kit string) error {
	if strings.TrimSpace(kit) == "" {
		return fmt.Errorf("%w: no StackKit named", ErrMultiNodeJoinUnsupported)
	}
	info, err := l.GetKitInfo(kit)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMultiNodeJoinUnsupported, err)
	}
	if !slices.Contains(info.Features, MultiNodeFeature) {
		return fmt.Errorf("%w: %s %s", ErrMultiNodeJoinUnsupported, info.Name, info.Version)
	}
	return nil
}

func stackKitInfoFromMetadata(kit stackkitrelease.KitMetadata) *StackKitInfo {
	metadata := kit.Metadata
	info := &StackKitInfo{
		Name: metadata.Name, DisplayName: metadata.DisplayName, Version: metadata.Version,
		Description: metadata.Description, Author: metadata.Author, License: metadata.License,
		Maturity: metadata.Maturity, Status: metadata.Status,
		Tags: metadata.Tags, Deprecated: kit.Deprecated, SupportedOS: kit.SupportedOS, Features: kit.Features,
	}
	if len(kit.Services.Required)+len(kit.Services.Recommended)+len(kit.Services.Available) > 0 {
		info.Services = &StackKitServicesInfo{Required: kit.Services.Required, Recommended: kit.Services.Recommended, Available: kit.Services.Available}
	}
	if len(kit.Modes) > 0 {
		info.Modes = make(map[string]StackKitModeInfo, len(kit.Modes))
		for name, mode := range kit.Modes {
			info.Modes[name] = StackKitModeInfo{Description: mode.Description, Engine: mode.Engine, TemplateDir: mode.TemplateDir, Requires: mode.Requires, RecommendedFor: mode.RecommendedFor}
		}
	}
	return info
}
