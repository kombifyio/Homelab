import { watchCompanionPanel } from "#lib/client/companion-bridge.js";

/**
 * Whether the in-app Companion gives way to Kubi on the desktop (AppLink v2 "In-app Companion"),
 * as the Windows client last said. False everywhere else, so the panel shows as before.
 */
export const companionPanel = $state({ hidden: false });

/** Starts listening for this document; returns the stop function. */
export function startCompanionPanel(): () => void {
  return watchCompanionPanel((hidden) => {
    companionPanel.hidden = hidden;
  });
}
