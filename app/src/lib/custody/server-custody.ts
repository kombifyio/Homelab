import type { StackOperationServer } from "#lib/api/stacks.js";

const providerControlAuthorities = new Set([
  "homelab_provider_control",
  "techstack_provider_control",
]);

function normalized(value: string | undefined): string {
  return value?.trim().toLowerCase() ?? "";
}

/**
 * Returns true only when the backend explicitly classified this concrete
 * server as legacy-quarantined or unbound. Missing or provider-controlled
 * authority never opts the UI into the owner-confirmed archive action.
 */
export function isLegacyOrUnboundCustody(
  server: Pick<StackOperationServer, "capabilities">,
): boolean {
  const authority = normalized(server.capabilities?.execution_authority);
  const state = normalized(server.capabilities?.authority_state);
  return (
    !providerControlAuthorities.has(authority) &&
    (state === "legacy_quarantined" || state === "unbound")
  );
}
