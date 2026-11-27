import type { DetectedExistingApp } from "#lib/api/unifier.js";

/** An existing app the Smart Home use case can take over (Home Assistant). */
export function isSmartHomeApp(app: DetectedExistingApp): boolean {
  return app.use_case_slug === "smart-home" || app.app_id === "home-assistant";
}
