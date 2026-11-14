/**
 * A run that names the shown distribution is revalidated before anything
 * changes (`internal/routes/stacks/wizard_layout.go`). When the homelab
 * changed in between, the run is refused and nothing is changed; the flow
 * then shows the replacement it was handed, or asks for the recommendation
 * again, so the person confirms the current distribution and not the old one.
 * Both flows with a distribution (add a server, add use cases) end here.
 */
import {
  layoutErrorReasonCode,
  layoutReplacementFromError,
  type LayoutRecommendationView,
} from "#lib/api/layout.js";
import type { CreationLayoutFlow } from "./core/processes";
import { tr } from "#lib/i18n.svelte.js";

export interface LayoutRefusal {
  notice: {
    tone: "warning" | "info";
    title: string;
    body: string;
  };
  /** The updated distribution the refusal carried, shown in place of the old one. */
  replacement?: LayoutRecommendationView;
  /** Ask for the recommendation again: the refusal carried no replacement. */
  refresh: boolean;
  /**
   * The confirmation that follows is another request, so it is a new attempt:
   * a flow whose attempt key does not follow the request ends the refused one.
   * A transient refusal keeps the attempt for a plain retry.
   */
  newAttempt: boolean;
}

/** What a refused run means for the shown distribution; null for any other failure. */
export function layoutRefusal(
  err: unknown,
  flow: CreationLayoutFlow,
): LayoutRefusal | null {
  const again = flow === "add-server" ? "addServer" : "addUseCases";
  switch (layoutErrorReasonCode(err)) {
    case "layout_recommendation_superseded": {
      const replacement = layoutReplacementFromError(err);
      return {
        notice: {
          tone: "warning",
          title: tr("layout.addUseCases.superseded.title"),
          body: tr(`layout.${again}.superseded.body`),
        },
        replacement,
        refresh: !replacement,
        newAttempt: true,
      };
    }
    case "layout_recommendation_stale":
    case "layout_recommendation_unverifiable":
      return {
        notice: {
          tone: "warning",
          title: tr("layout.addUseCases.stale.title"),
          body: tr(`layout.${again}.stale.body`),
        },
        refresh: true,
        newAttempt: true,
      };
    case "layout_recommendation_unavailable":
    case "layout_recommendations_unavailable":
      return {
        notice: {
          tone: "info",
          title: tr("layout.addUseCases.unavailable.title"),
          body: tr("layout.addUseCases.unavailable.body"),
        },
        refresh: false,
        newAttempt: false,
      };
    default:
      return null;
  }
}
