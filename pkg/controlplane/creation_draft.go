package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// CreationDraft is where one owner stands in one creation process
// (KOMBIFY-DESIGN-SYSTEM-STANDARD.md section 9): the current and furthest
// step and the non-secret answers. Every frame of the creation flow reads it,
// so a creation started in the Cloud popup continues on the Techstack page or
// on another device. It ends on an accepted create, an explicit discard or
// its expiry.
type CreationDraft struct {
	ID             string
	TenantID       string
	OwnerSubjectID string
	// Key names the process and its target as the wizard does
	// (stack-create, add-use-case-<stack>, ...).
	Key       string
	Process   string
	Step      string
	Furthest  string
	Answers   json.RawMessage
	ExpiresAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreationDraftStore keeps one draft per tenant, owner and key. A read after
// the expiry finds nothing; a write replaces the whole draft and removes the
// owner's expired drafts.
type CreationDraftStore interface {
	GetCreationDraft(ctx context.Context, tenantID, ownerSubjectID, key string) (*CreationDraft, error)
	PutCreationDraft(ctx context.Context, draft CreationDraft) (*CreationDraft, error)
	DeleteCreationDraft(ctx context.Context, tenantID, ownerSubjectID, key string) error
}

// CreationDraftTTL is how long a draft outlives its last write.
const CreationDraftTTL = 7 * 24 * time.Hour

// MaxCreationDraftAnswersBytes bounds the stored answers.
const MaxCreationDraftAnswersBytes = 48 << 10

var (
	creationDraftKeyPattern = regexp.MustCompile(`^[a-z][A-Za-z0-9_.:-]{0,191}$`)
	// The process keys of the creation flow (app/src/lib/creation/core/processes.ts).
	creationDraftProcesses = map[string]bool{
		"stack": true, "place-on-server": true, "add-server": true, "add-server-found": true,
		"join-on-server": true, "add-use-case": true, "homelab-settings": true,
	}
	creationDraftSteps = map[string]int{"goals": 1, "server": 2, "access": 3, "users": 4, "login": 5}
)

var (
	// ErrCreationDraftInvalid refuses a draft outside the closed contract.
	ErrCreationDraftInvalid = errors.New("creation draft is not valid")
	// ErrCreationDraftSecret refuses answers that carry a secret. The wizard
	// blanks passwords and passphrase hashes before it writes; a field that
	// still holds one is refused, never stored or stripped silently.
	ErrCreationDraftSecret = errors.New("creation draft carries a secret")
)

// ValidCreationDraftKey reports whether key may name a draft.
func ValidCreationDraftKey(key string) bool {
	return creationDraftKeyPattern.MatchString(key)
}

func normalizeCreationDraft(draft CreationDraft) (CreationDraft, error) {
	out := draft
	out.ID = strings.TrimSpace(out.ID)
	out.TenantID = strings.TrimSpace(out.TenantID)
	out.OwnerSubjectID = strings.TrimSpace(out.OwnerSubjectID)
	if out.ID == "" || out.TenantID == "" || out.OwnerSubjectID == "" {
		return out, fmt.Errorf("%w: id, tenant and owner are required", ErrCreationDraftInvalid)
	}
	if !ValidCreationDraftKey(out.Key) {
		return out, fmt.Errorf("%w: key %q", ErrCreationDraftInvalid, out.Key)
	}
	if !creationDraftProcesses[out.Process] {
		return out, fmt.Errorf("%w: process %q", ErrCreationDraftInvalid, out.Process)
	}
	step, stepOK := creationDraftSteps[out.Step]
	furthest, furthestOK := creationDraftSteps[out.Furthest]
	if !stepOK || !furthestOK {
		return out, fmt.Errorf("%w: step %q or furthest %q", ErrCreationDraftInvalid, out.Step, out.Furthest)
	}
	if furthest < step {
		out.Furthest = out.Step
	}
	if len(out.Answers) > MaxCreationDraftAnswersBytes {
		return out, fmt.Errorf("%w: answers exceed %d bytes", ErrCreationDraftInvalid, MaxCreationDraftAnswersBytes)
	}
	var answers any
	if err := json.Unmarshal(out.Answers, &answers); err != nil {
		return out, fmt.Errorf("%w: answers are not JSON", ErrCreationDraftInvalid)
	}
	if _, ok := answers.(map[string]any); !ok {
		return out, fmt.Errorf("%w: answers must be an object", ErrCreationDraftInvalid)
	}
	if path := creationDraftSecretPath(answers, ""); path != "" {
		return out, fmt.Errorf("%w: %s", ErrCreationDraftSecret, strings.TrimPrefix(path, "."))
	}
	return out, nil
}

// creationDraftSecretPath returns the path of the first field whose name
// marks a secret and whose value is not empty. Booleans and numbers never
// carry one (requirePassword, sshPort), and an empty string is a blanked
// field.
func creationDraftSecretPath(value any, path string) string {
	switch typed := value.(type) {
	case map[string]any:
		for name, child := range typed {
			childPath := path + "." + name
			if creationDraftSecretName(name) && carriesSecretValue(child) {
				return childPath
			}
			if found := creationDraftSecretPath(child, childPath); found != "" {
				return found
			}
		}
	case []any:
		for index, child := range typed {
			if found := creationDraftSecretPath(child, fmt.Sprintf("%s[%d]", path, index)); found != "" {
				return found
			}
		}
	}
	return ""
}

var creationDraftSecretSuffixes = []string{
	"password", "passwd", "passphrase", "passphrasehash", "secret", "token",
	"privatekey", "apikey", "authkey", "preauthkey", "credential", "credentials",
	"recoverymaterial", "recoverycode", "recoverycodes", "totp",
}

func creationDraftSecretName(name string) bool {
	normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(name))
	for _, suffix := range creationDraftSecretSuffixes {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}

func carriesSecretValue(value any) bool {
	switch typed := value.(type) {
	case nil, bool, float64:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case map[string]any:
		return len(typed) > 0
	case []any:
		return len(typed) > 0
	default:
		return true
	}
}
