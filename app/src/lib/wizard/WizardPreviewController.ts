import {
  recommendWizard,
  type WizardRecommendationRequest,
  type WizardRecommendationResult,
} from "#lib/api/unifier.js";

import { tr } from "#lib/i18n.svelte.js";
/**
 * `key` names the request an answer belongs to (`wizardRequestKey`): a client
 * compares it with the key of what is on screen now, so an answer to an earlier
 * question is never shown as the answer to this one.
 */
export type WizardPreviewState =
  | { phase: "idle" }
  | { phase: "loading"; key: string; previous?: WizardRecommendationResult }
  | { phase: "resolved"; key: string; result: WizardRecommendationResult }
  | {
      phase: "degraded";
      key: string;
      previous?: WizardRecommendationResult;
      message: string;
    };

/** The identity of a request as the controller asks it. */
export function wizardRequestKey(request: WizardRecommendationRequest): string {
  return JSON.stringify(normalizeRequest(request));
}

export type WizardRecommendationLoader = (
  request: WizardRecommendationRequest,
  signal: AbortSignal,
) => Promise<WizardRecommendationResult>;

export class WizardPreviewController {
  readonly #load: WizardRecommendationLoader;
  readonly #onchange: (state: WizardPreviewState) => void;
  readonly #debounceMs: number;
  readonly #cache = new Map<string, WizardRecommendationResult>();
  #timer: ReturnType<typeof setTimeout> | null = null;
  #request: AbortController | null = null;
  #sequence = 0;
  #current: WizardRecommendationResult | undefined;

  constructor(
    onchange: (state: WizardPreviewState) => void,
    load: WizardRecommendationLoader = recommendWizard,
    debounceMs = 250,
  ) {
    this.#onchange = onchange;
    this.#load = load;
    this.#debounceMs = debounceMs;
  }

  update(request: WizardRecommendationRequest, options?: { force?: boolean }): void {
    const normalized = normalizeRequest(request);
    const key = JSON.stringify(normalized);
    this.cancelPending();
    const sequence = ++this.#sequence;
    const isObservedFit = Boolean(
      normalized.existing_server_id ||
        (normalized.server_fit && !("provider_id" in normalized.server_fit)),
    );
    const cached =
      options?.force || isObservedFit ? undefined : this.#cache.get(key);
    if (cached) {
      this.#current = cached;
      this.#onchange({ phase: "resolved", key, result: cached });
      return;
    }

    this.#onchange({ phase: "loading", key, previous: this.#current });
    const delay = options?.force ? 0 : this.#debounceMs;
    this.#timer = setTimeout(() => {
      this.#timer = null;
      const controller = new AbortController();
      this.#request = controller;
      void this.#load(normalized, controller.signal)
        .then((result) => {
          if (controller.signal.aborted || sequence !== this.#sequence) return;
          if (!isObservedFit) this.#cache.set(key, result);
          this.#current = result;
          this.#onchange({ phase: "resolved", key, result });
        })
        .catch((error: unknown) => {
          if (controller.signal.aborted || sequence !== this.#sequence) return;
          this.#onchange({
            phase: "degraded",
            key,
            previous: this.#current,
            message:
              error instanceof Error
                ? error.message
                : tr(
                    "ui.wizardPreviewController.recommendationPreviewIsTemporarilyUnavailable",
                  ),
          });
        })
        .finally(() => {
          if (this.#request === controller) this.#request = null;
        });
    }, delay);
  }

  refresh(request: WizardRecommendationRequest): void {
    this.update(request, { force: true });
  }

  destroy(): void {
    this.#sequence++;
    this.cancelPending();
    this.#cache.clear();
    this.#current = undefined;
  }

  private cancelPending(): void {
    if (this.#timer) {
      clearTimeout(this.#timer);
      this.#timer = null;
    }
    this.#request?.abort();
    this.#request = null;
  }
}

function normalizeRequest(
  request: WizardRecommendationRequest,
): WizardRecommendationRequest {
  const goals = normalizeList(request.goals);
  const services = normalizeList(request.services);
  const provider = request.provider_id?.trim();
  return {
    ...(request.smart_home_context
      ? { smart_home_context: request.smart_home_context }
      : {}),
    ...(request.smart_home_settings
      ? { smart_home_settings: request.smart_home_settings }
      : {}),
    ...(request.existing_server_id?.trim()
      ? { existing_server_id: request.existing_server_id.trim() }
      : {}),
    goals,
    services,
    deployment_lane: request.deployment_lane,
    ...(provider ? { provider_id: provider } : {}),
    ...(request.server_fit ? { server_fit: request.server_fit } : {}),
    ...(request.compute_profile
      ? { compute_profile: request.compute_profile.trim() }
      : {}),
    ...(request.use_case_settings
      ? { use_case_settings: request.use_case_settings }
      : {}),
    ...(request.use_case_capabilities
      ? { use_case_capabilities: request.use_case_capabilities }
      : {}),
    ...(request.server_capacity
      ? { server_capacity: request.server_capacity }
      : {}),
    ...(request.runtime_offering_id?.trim()
      ? { runtime_offering_id: request.runtime_offering_id.trim() }
      : {}),
    ...(request.kit_slug?.trim() ? { kit_slug: request.kit_slug.trim() } : {}),
    surface: request.surface,
  };
}

function normalizeList(values: string[]): string[] {
  return [
    ...new Set(values.map((value) => value.trim()).filter(Boolean)),
  ].sort();
}
