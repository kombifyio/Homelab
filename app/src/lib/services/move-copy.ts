import { tr } from "#lib/i18n.svelte.js";
import type {
  RegistryMoveIneligibleCode,
  RegistryMoveWarning,
} from "#lib/api/registry.js";

/**
 * Localized copy for the codes the backend reports on a move. The backend
 * `reason` and `message` fields are English diagnostics: an unknown code gets
 * the generic localized fallback, never that text.
 */
export function moveIneligibleLabel(code: RegistryMoveIneligibleCode): string {
  switch (code) {
    case "insufficient_capacity":
      return tr("applicationMove.ineligible.insufficient_capacity");
    case "local_hardware_bound":
      return tr("applicationMove.ineligible.local_hardware_bound");
    case "unsupported_adapter":
      return tr("applicationMove.ineligible.unsupported_adapter");
    case "multi_app_use_case":
      return tr("applicationMove.ineligible.multi_app_use_case");
    case "same_stack_deployment":
      return tr("applicationMove.ineligible.same_stack_deployment");
    case "destination_not_relocatable":
      return tr("applicationMove.ineligible.destination_not_relocatable");
    case "server_not_ready":
      return tr("applicationMove.ineligible.server_not_ready");
    default:
      return tr("applicationMove.ineligible.generic");
  }
}

export function moveWarningLabel(code: RegistryMoveWarning["code"]): string {
  switch (code) {
    case "direct_transfer_network_required":
      return tr("applicationMove.warning.direct_transfer_network_required");
    case "encrypted_relay_transport":
      return tr("applicationMove.info.encrypted_relay_transport");
    default:
      return tr("applicationMove.warning.generic");
  }
}
