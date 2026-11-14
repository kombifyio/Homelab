// Package primitives is Techstack's exact copy of the executor-bound RIL
// primitive set of the StackKits catalog (foundation/architecture_v2_catalog.cue,
// support "executor-bound"). Every authority seam (card admission, Gateway
// lease and receipt checks, the dispatcher and the node proof) binds a
// primitive through this table; there is no wildcard and no other primitive.
//
// The Gateway carries the same table in cloudflare-edge/src/ril-primitive-profiles.ts
// and the workspace contract in platform/standards/ril-effect.v1.schema.json.
package primitives

import (
	"strings"

	"github.com/kombifyio/techstack/pkg/api/agentpb"
	rilaction "github.com/kombifyio/techstack/pkg/ril/actioncontract"
)

// Input identifiers and their opaque-reference prefixes.
const (
	InputRuntimeInstanceRef = "runtime-instance-ref" // object:service/<service key>
	InputTargetReleaseRef   = "target-release-ref"   // object:release/<StackKits release version>
	InputBackupContractRef  = "backup-contract-ref"  // object:backup/<stack id>
	// InputApplicationRef is the optional input of the service power
	// primitives that names an adopted application's exact container.
	InputApplicationRef = "application-ref" // object:application/<container id>
	// InputHostContainerRef and InputHostUnitRef name the one object of a host
	// primitive: an unmanaged container (name or id) or a .service unit.
	InputHostContainerRef = "host-container-ref" // object:container/<container name or id>
	InputHostUnitRef      = "host-unit-ref"      // object:unit/<unit>.service

	ServiceRefPrefix     = "object:service/"
	ReleaseRefPrefix     = "object:release/"
	BackupRefPrefix      = "object:backup/"
	ApplicationRefPrefix = "object:application/"
	HostContainerPrefix  = "object:container/"
	HostUnitPrefix       = "object:unit/"
)

// EquipmentPolicyPrefix names the Decision principal that answers an
// executor-bound primitive: the acting agent's equipment (D-2026-10-23-06),
// never a per-use person prompt. The full id is EquipmentPolicyPrefix+GrantScope.
const EquipmentPolicyPrefix = "equipment:"

// DecisionOperation is the one Decision intent operation of the action-card path.
const DecisionOperation = "techstack.ril.execute"

// Profile is the exact binding of one executor-bound primitive.
type Profile struct {
	ID             string
	OperationClass string
	GrantScope     string
	TargetScope    rilaction.TargetScope
	// Inputs are the exact, sorted required catalog input ids; every one is
	// an opaque reference.
	Inputs []string
	// OptionalInputs are the catalog's optional opaque-reference inputs. The
	// only one is application-ref of the service power primitives: present
	// exactly when the service is an adopted application.
	OptionalInputs []string
	// Mutation is true for primitives that change the server.
	Mutation bool
	// GuardOperation is the one typed StackKits command the node runs for a
	// managed service, stack or backup target.
	GuardOperation agentpb.StackKitOperation
	// ApplicationAction is the application control power action
	// (start, stop or restart) the node runs instead of GuardOperation when
	// the effect names an adopted application (application-ref); "" when the
	// primitive never covers one.
	ApplicationAction string
	// HostObject is "container" or "unit" for a host primitive, and HostAction
	// its one verb (start, stop or restart): the Guard runs exactly that verb on
	// exactly the object the effect names. Both are "" for every other
	// primitive.
	HostObject string
	HostAction string
}

var profiles = []Profile{
	{ID: "check-backup", OperationClass: "backup-evidence", GrantScope: "stackkit-backup-check", TargetScope: rilaction.TargetScopeModuleInstance,
		Inputs: []string{InputBackupContractRef}, GuardOperation: agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_STATUS},
	{ID: "restart-host-container", OperationClass: "host-container-restart", GrantScope: "stackkit-host-container-restart", TargetScope: rilaction.TargetScopeRuntimeInstance,
		Inputs: []string{InputHostContainerRef}, Mutation: true, GuardOperation: agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_CONTAINER_ACTION, HostObject: "container", HostAction: "restart"},
	{ID: "restart-host-unit", OperationClass: "host-unit-restart", GrantScope: "stackkit-host-unit-restart", TargetScope: rilaction.TargetScopeRuntimeInstance,
		Inputs: []string{InputHostUnitRef}, Mutation: true, GuardOperation: agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_SYSTEMD_ACTION, HostObject: "unit", HostAction: "restart"},
	{ID: "restart-service", OperationClass: "runtime-service-action", GrantScope: "stackkit-service-restart", TargetScope: rilaction.TargetScopeRuntimeInstance,
		Inputs: []string{InputRuntimeInstanceRef}, OptionalInputs: []string{InputApplicationRef}, Mutation: true,
		GuardOperation: agentpb.StackKitOperation_STACKKIT_OPERATION_SERVICE_RESTART, ApplicationAction: "restart"},
	{ID: "run-backup", OperationClass: "backup-run", GrantScope: "stackkit-backup-run", TargetScope: rilaction.TargetScopeModuleInstance,
		Inputs: []string{InputBackupContractRef}, Mutation: true, GuardOperation: agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RUN},
	{ID: "start-host-container", OperationClass: "host-container-start", GrantScope: "stackkit-host-container-start", TargetScope: rilaction.TargetScopeRuntimeInstance,
		Inputs: []string{InputHostContainerRef}, Mutation: true, GuardOperation: agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_CONTAINER_ACTION, HostObject: "container", HostAction: "start"},
	{ID: "start-host-unit", OperationClass: "host-unit-start", GrantScope: "stackkit-host-unit-start", TargetScope: rilaction.TargetScopeRuntimeInstance,
		Inputs: []string{InputHostUnitRef}, Mutation: true, GuardOperation: agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_SYSTEMD_ACTION, HostObject: "unit", HostAction: "start"},
	{ID: "start-service", OperationClass: "runtime-service-start", GrantScope: "stackkit-service-start", TargetScope: rilaction.TargetScopeRuntimeInstance,
		Inputs: []string{InputRuntimeInstanceRef}, OptionalInputs: []string{InputApplicationRef}, Mutation: true,
		GuardOperation: agentpb.StackKitOperation_STACKKIT_OPERATION_SERVICE_START, ApplicationAction: "start"},
	{ID: "stop-host-container", OperationClass: "host-container-stop", GrantScope: "stackkit-host-container-stop", TargetScope: rilaction.TargetScopeRuntimeInstance,
		Inputs: []string{InputHostContainerRef}, Mutation: true, GuardOperation: agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_CONTAINER_ACTION, HostObject: "container", HostAction: "stop"},
	{ID: "stop-host-unit", OperationClass: "host-unit-stop", GrantScope: "stackkit-host-unit-stop", TargetScope: rilaction.TargetScopeRuntimeInstance,
		Inputs: []string{InputHostUnitRef}, Mutation: true, GuardOperation: agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_SYSTEMD_ACTION, HostObject: "unit", HostAction: "stop"},
	{ID: "stop-service", OperationClass: "runtime-service-stop", GrantScope: "stackkit-service-stop", TargetScope: rilaction.TargetScopeRuntimeInstance,
		Inputs: []string{InputRuntimeInstanceRef}, OptionalInputs: []string{InputApplicationRef}, Mutation: true,
		GuardOperation: agentpb.StackKitOperation_STACKKIT_OPERATION_SERVICE_STOP, ApplicationAction: "stop"},
	{ID: "update-service", OperationClass: "runtime-service-update", GrantScope: "stackkit-service-update", TargetScope: rilaction.TargetScopeRuntimeInstance,
		Inputs: []string{InputRuntimeInstanceRef, InputTargetReleaseRef}, Mutation: true, GuardOperation: agentpb.StackKitOperation_STACKKIT_OPERATION_UPGRADE},
	{ID: "verify-stackkit-state", OperationClass: "product-verify", GrantScope: "stackkit-verify", TargetScope: rilaction.TargetScopeStack,
		GuardOperation: agentpb.StackKitOperation_STACKKIT_OPERATION_VERIFY},
}

// Lookup returns the profile of an executor-bound primitive id.
func Lookup(id string) (Profile, bool) {
	for _, profile := range profiles {
		if profile.ID == id {
			return profile, true
		}
	}
	return Profile{}, false
}

// For returns the profile only when the primitive binding names exactly an
// executor-bound primitive with its catalog operation class.
func For(binding rilaction.PrimitiveBinding) (Profile, bool) {
	profile, ok := Lookup(binding.ID)
	if !ok || binding.OperationClass != profile.OperationClass {
		return Profile{}, false
	}
	return profile, true
}

// All returns a copy of the executor-bound set.
func All() []Profile {
	out := make([]Profile, len(profiles))
	for i, profile := range profiles {
		out[i] = profile
		out[i].Inputs = append([]string(nil), profile.Inputs...)
		out[i].OptionalInputs = append([]string(nil), profile.OptionalInputs...)
	}
	return out
}

// EquipmentPolicyID is the Decision principal id that answers this primitive.
func (p Profile) EquipmentPolicyID() string { return EquipmentPolicyPrefix + p.GrantScope }

// Admits reports whether a request shape matches this profile exactly: the
// target scope and the inputs sorted by id, every required one present, at
// most the profile's optional ones besides, each a non-empty opaque reference
// with its catalog prefix.
func (p Profile) Admits(target rilaction.TargetBinding, inputs []rilaction.Input) bool {
	if target.Scope != p.TargetScope || len(inputs) < len(p.Inputs) || len(inputs) > len(p.Inputs)+len(p.OptionalInputs) {
		return false
	}
	required := 0
	for i, in := range inputs {
		if i > 0 && inputs[i-1].ID >= in.ID {
			return false
		}
		switch {
		case containsID(p.Inputs, in.ID):
			required++
		case containsID(p.OptionalInputs, in.ID):
		default:
			return false
		}
		if in.Type != rilaction.InputTypeOpaqueReference || in.OpaqueRef == "" || in.Boolean != nil || in.Integer != nil || in.Enum != "" {
			return false
		}
		if !strings.HasPrefix(in.OpaqueRef, inputPrefix(in.ID)) || len(in.OpaqueRef) == len(inputPrefix(in.ID)) {
			return false
		}
	}
	return required == len(p.Inputs)
}

// GuardOperationFor is the one typed StackKits command the node runs for a
// shape this profile admits: application control when the inputs name an
// adopted application, otherwise the profile's Guard operation.
func (p Profile) GuardOperationFor(inputs []rilaction.Input) agentpb.StackKitOperation {
	if p.ApplicationAction != "" && InputValue(inputs, InputApplicationRef) != "" {
		return agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_CONTROL
	}
	return p.GuardOperation
}

// AdmitsGuardOperation reports whether op is one of the typed commands this
// primitive may run: its Guard operation, or application control for a
// service power primitive.
func (p Profile) AdmitsGuardOperation(op agentpb.StackKitOperation) bool {
	return op == p.GuardOperation || (p.ApplicationAction != "" && op == agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_CONTROL)
}

func containsID(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

// IsExecutorBound reports whether a primitive id names any executor-bound
// primitive, whatever operation class a caller paired it with. A known id with
// another class is a mismatch, never a contract-only primitive.
func IsExecutorBound(id string) bool {
	_, ok := Lookup(id)
	return ok
}

// Input returns the one required opaque-reference input id with its catalog
// prefix and value.
func Input(id, value string) rilaction.Input {
	return rilaction.Input{ID: id, Type: rilaction.InputTypeOpaqueReference, OpaqueRef: inputPrefix(id) + value}
}

// BackupModuleInstanceRef is the module-instance ref of a stack's backup
// contract (the target of check-backup and run-backup).
func BackupModuleInstanceRef(stackID string) string { return "backup." + stackID }

// InputValue returns the value after the input's prefix, or "".
func InputValue(inputs []rilaction.Input, id string) string {
	for _, in := range inputs {
		if in.ID == id && strings.HasPrefix(in.OpaqueRef, inputPrefix(id)) {
			return strings.TrimPrefix(in.OpaqueRef, inputPrefix(id))
		}
	}
	return ""
}

func inputPrefix(id string) string {
	switch id {
	case InputHostContainerRef:
		return HostContainerPrefix
	case InputHostUnitRef:
		return HostUnitPrefix
	case InputRuntimeInstanceRef:
		return ServiceRefPrefix
	case InputTargetReleaseRef:
		return ReleaseRefPrefix
	case InputBackupContractRef:
		return BackupRefPrefix
	case InputApplicationRef:
		return ApplicationRefPrefix
	}
	return "\x00"
}

// IsHost reports a host primitive: one verb on one unmanaged container or
// service unit of the connected server's host, run by the Guard itself.
func (p Profile) IsHost() bool { return p.HostObject != "" }

// HostInput is the profile's one required object input id.
func (p Profile) HostInput() string {
	switch p.HostObject {
	case "container":
		return InputHostContainerRef
	case "unit":
		return InputHostUnitRef
	}
	return ""
}

// ForHost returns the host primitive of one object kind ("container" or
// "unit") and verb; there is no other way to name one.
func ForHost(object, action string) (Profile, bool) {
	for _, profile := range profiles {
		if profile.HostObject == object && profile.HostAction == action && object != "" {
			return profile, true
		}
	}
	return Profile{}, false
}
