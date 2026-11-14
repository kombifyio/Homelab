/**
 * What the access decision of one use case starts from: the homelab-wide
 * default it inherits, the decision saved earlier, and the household people
 * the homelab already knows (the same reads the dashboard People panel uses).
 * Nothing here invents a person or an identity.
 */
import { parseApiError } from "#lib/api/errors.js";
import { getHomelab } from "#lib/api/homelab.js";
import { loadPeople } from "#lib/dashboard/people.js";
import {
  storedHomelabAccess,
  storedUseCaseAccess,
} from "#lib/wizard/stored-decisions.js";
import type { UseCaseAccessChoice } from "#lib/wizard/types.js";

/**
 * The signed feature flag for access per use case. The pinned release enforces
 * local Photos/Documents audiences; when off, no choice is shown or sent.
 */
export const USE_CASE_ACCESS_FLAG = "use_case_access";

export interface HouseholdPersonChoice {
  /** The planned person's reference, or the member's homelab user name. */
  ref: string;
  name: string;
}

export interface HomelabAccessFacts {
  mode?: "local" | "remote-private";
  audience?: "owner" | "household";
  saved: Record<string, UseCaseAccessChoice>;
  people: HouseholdPersonChoice[];
}

export async function loadHomelabAccess(): Promise<HomelabAccessFacts> {
  const view = await getHomelab();
  const people = await loadPeople(
    (view?.kit_deployments ?? []).map((deployment) => deployment.id),
    "",
  );
  return {
    ...storedHomelabAccess(view?.homelab),
    saved: storedUseCaseAccess(view?.homelab),
    people:
      people.state === "ready"
        ? people.items
            .filter((person) => person.audience === "household")
            .map((person) => ({
              ref: person.id.replace(/^person:(planned:)?/, ""),
              name: person.name,
            }))
        : [],
  };
}

/**
 * The use cases whose own access blocked a run: the decision is saved and
 * nothing was rolled out. Empty for any other answer.
 */
export function useCaseAccessBlockedGoals(error: unknown): string[] {
  const direct = parseApiError(error).details;
  const nested = (direct as { details?: unknown } | null)?.details;
  const details =
    typeof nested === "object" && nested !== null ? nested : direct;
  if (
    typeof details !== "object" ||
    details === null ||
    (details as { reason_code?: unknown }).reason_code !==
      "wizard_desired_projection_blocked"
  )
    return [];
  const blocked = (details as { blocked_capabilities?: unknown })
    .blocked_capabilities;
  if (!Array.isArray(blocked)) return [];
  return [
    ...new Set(
      blocked.flatMap((entry) =>
        entry?.capability === "use-case-access" &&
        typeof entry.use_case === "string"
          ? [entry.use_case]
          : [],
      ),
    ),
  ];
}
