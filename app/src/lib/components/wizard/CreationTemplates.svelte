<script lang="ts">
  import { Rocket, FolderOpen, Briefcase, House } from "@lucide/svelte";
  import type {
    CreationTemplate,
    CreationTemplateIcon,
    CreationTemplateId,
    TemplateSelection,
  } from "#lib/wizard/creationTemplates.js";
  import type {
    ProStarterAlternative,
    ProStarterEvaluation,
  } from "#lib/api/unifier.js";
  import type { CreationGoal } from "#lib/wizard/creationGoals.js";
  import { tr } from "#lib/i18n.svelte.js";

  /**
   * The profile row above the use cases. Each profile switches its own fixed
   * set of use cases on, and a second click switches it off again without
   * touching what another profile that is on holds. Several may be on at once;
   * each shows its own colour, which the rings of its use cases repeat.
   */
  let {
    templates,
    live,
    ontoggle,
    standardStart,
    expansion,
    proStarters,
    cards,
    onalternative,
  }: {
    templates: { template: CreationTemplate; selection: TemplateSelection }[];
    /** The profiles that are on. */
    live: readonly CreationTemplateId[];
    ontoggle: (id: CreationTemplateId) => void;
    standardStart: boolean;
    expansion: Record<CreationTemplateId, { expansion: readonly string[] }>;
    proStarters?: Record<string, ProStarterEvaluation>;
    cards: CreationGoal[];
    onalternative?: (alt: ProStarterAlternative) => void;
  } = $props();

  let confirmedProfiles = $state<Record<string, boolean>>({});
  const confirmationResetKey = $derived(
    JSON.stringify(
      Object.keys(proStarters ?? {})
        .sort()
        .map((id) => {
          const starter = proStarters?.[id];
          return [
            id,
            starter?.recommended_full ?? false,
            starter?.requires_confirmation ?? false,
            starter?.confirmation_reasons ?? [],
            starter?.fit?.provider_id ?? "",
            starter?.fit?.offering_id ?? "",
            starter?.fit?.server_id ?? "",
            starter?.fit?.status ?? "",
            (starter?.alternatives ?? []).map((alt) => [
              alt.kind,
              alt.goal ?? "",
              alt.profile ?? "",
              alt.server_id ?? "",
              alt.provider_id ?? "",
              alt.offering_id ?? "",
            ]),
          ];
        }),
    ),
  );
  $effect(() => {
    void confirmationResetKey;
    confirmedProfiles = {};
  });
  const reasonMessages: Record<string, string> = {
    documents_media_host_insufficient:
      "wizard.templates.reason.documents_media_host_insufficient",
    smart_home_deferred_lan_reach:
      "wizard.templates.reason.smart_home_deferred_lan_reach",
    smart_home_deferred_host_capacity:
      "wizard.templates.reason.smart_home_deferred_host_capacity",
    local_ai_requires_suitable_host:
      "wizard.templates.reason.local_ai_requires_suitable_host",
    provider_package_unavailable:
      "wizard.templates.reason.provider_package_unavailable",
  };

  function providerDisplayName(providerId?: string): string {
    if (providerId === "centron") return "Centron";
    if (providerId === "ionos") return "IONOS";
    return providerId || "";
  }

  // Templates name their icon in data, so a new template needs no change here.
  const icons = {
    house: House,
    folder: FolderOpen,
    briefcase: Briefcase,
    rocket: Rocket,
  } satisfies Record<CreationTemplateIcon, unknown>;
</script>

{#if templates.length > 0}
  <div
    class="creation-templates"
    role="group"
    aria-label={tr("wizard.templates.title")}
    data-testid="creation-templates"
  >
    {#each templates as { template, selection } (template.id)}
      {@const Icon = icons[template.icon]}
      <button
        type="button"
        class="template"
        style:--template-color={template.color}
        aria-pressed={live.includes(template.id)}
        title={selection.skipped.length > 0
          ? `${tr(template.descriptionKey)} · ${tr("wizard.templates.notYetAvailable", { count: selection.skipped.length })}`
          : tr(template.descriptionKey)}
        data-testid={`creation-template-${template.id}`}
        onclick={() => ontoggle(template.id)}
      >
        <Icon size={15} aria-hidden="true" />
        <span>{tr(template.titleKey)}</span>
      </button>
    {/each}
    {#if standardStart}
      {#each templates.filter( ({ template }) => live.includes(template.id) ) as { template, selection } (template.id)}
        {@const starter = proStarters?.[template.id]}
        {@const deferred = starter
          ? (starter.deferred_goals ?? [])
          : expansion[template.id].expansion}
        {@const selectedGoals = new Set<string>(selection.goals)}
        {@const later = [
          ...new Set([
            ...deferred,
            ...selection.skipped,
          ]),
        ].filter((id) => !selectedGoals.has(id))}
        <p
          class="expansion"
          data-testid={`creation-template-start-${template.id}`}
        >
          {tr("wizard.templates.startsWith")}: {selection.goals
            .map((goal) => {
              const card = cards.find(
                (item) => item.question?.configKey === goal,
              );
              return card?.question ? tr(card.question.titleKey) : goal;
            })
            .join(", ")}
          {#if starter?.recommended_full}
            · <strong>{tr("wizard.templates.recommendedFull")}</strong>
          {/if}
        </p>
        {#if later.length}
          <p
            class="expansion"
            data-testid={`creation-template-expansion-${template.id}`}
          >
            {tr("wizard.templates.expandLater")}: {later
              .map((id) => {
                const card = cards.find((item) => item.id === id);
                return card?.question
                  ? tr(card.question.titleKey)
                  : tr(`wizard.preview.${id}.title`);
              })
              .join(", ")}
          </p>
        {/if}
        {#if starter?.requires_confirmation && ((starter.confirmation_reasons?.length ?? 0) > 0 || (starter.alternatives?.length ?? 0) > 0)}
          <div
            class="starter-confirmation"
            data-testid={`creation-template-confirmation-${template.id}`}
          >
            {#each starter.confirmation_reasons ?? [] as reason (reason)}
              {#if reasonMessages[reason]}
                <p class="expansion-note">{tr(reasonMessages[reason])}</p>
              {/if}
            {/each}
            <label class="confirm-toggle">
              <input
                type="checkbox"
                data-testid={`creation-template-confirm-${template.id}`}
                checked={confirmedProfiles[template.id] === true}
                onchange={(event) => {
                  confirmedProfiles[template.id] = event.currentTarget.checked;
                }}
              />
              <span>
                {confirmedProfiles[template.id]
                  ? tr("wizard.templates.acceptedStarter")
                  : tr("wizard.templates.acceptStarter")}
              </span>
            </label>
            {#if onalternative && starter.alternatives?.length}
              <div class="starter-alternatives">
                {#each starter.alternatives as alt (`${alt.kind}:${alt.goal ?? ""}:${alt.profile ?? ""}:${alt.server_id ?? ""}:${alt.provider_id ?? ""}:${alt.offering_id ?? ""}`)}
                  {#if alt.kind === "use_owned_server" && alt.server_id && (!alt.requires_confirmation || confirmedProfiles[template.id])}
                    <button
                      type="button"
                      class="starter-alt-btn"
                      onclick={() => onalternative(alt)}
                    >
                      {tr("wizard.templates.alternative.useOwnedServer", {
                        server: alt.server_name || alt.server_id,
                      })}
                    </button>
                  {:else if alt.kind === "switch_provider" && alt.provider_id && (!alt.requires_confirmation || confirmedProfiles[template.id])}
                    <button
                      type="button"
                      class="starter-alt-btn"
                      onclick={() => onalternative(alt)}
                    >
                      {tr("wizard.templates.alternative.switchProvider", {
                        provider: alt.offering_sku
                          ? `${providerDisplayName(alt.provider_id)} (${alt.offering_sku})`
                          : providerDisplayName(alt.provider_id),
                      })}
                    </button>
                  {:else if alt.kind === "switch_offering" && alt.offering_id && (!alt.requires_confirmation || confirmedProfiles[template.id])}
                    <button
                      type="button"
                      class="starter-alt-btn"
                      onclick={() => onalternative(alt)}
                    >
                      {tr("wizard.templates.alternative.switchOffering", {
                        offering: alt.offering_sku || alt.offering_id,
                      })}
                    </button>
                  {:else if alt.kind === "switch_profile" && alt.profile && (!alt.requires_confirmation || confirmedProfiles[template.id])}
                    <button
                      type="button"
                      class="starter-alt-btn"
                      onclick={() => onalternative(alt)}
                    >
                      {tr("wizard.templates.alternative.switchProfile", {
                        profile: alt.profile,
                        lost: (alt.lost_functions ?? ["ai-search"]).join(", "),
                      })}
                    </button>
                  {/if}
                {/each}
              </div>
            {/if}
          </div>
        {/if}
      {/each}
    {/if}
  </div>
{/if}

<style>
  .creation-templates {
    display: flex;
    flex-wrap: wrap;
    justify-content: center;
    gap: 8px;
  }
  .expansion {
    width: 100%;
    text-align: center;
    color: var(--muted-foreground);
    font-size: 13px;
  }
  .starter-confirmation {
    width: 100%;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    color: var(--muted-foreground);
  }
  .expansion-note {
    text-align: center;
    max-width: 64ch;
  }
  .confirm-toggle {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    color: var(--foreground);
    cursor: pointer;
  }
  .starter-alternatives {
    display: flex;
    flex-wrap: wrap;
    justify-content: center;
    gap: 8px;
  }
  .starter-alt-btn {
    padding: 4px 10px;
    border-radius: 999px;
    border: 1px solid var(--border);
    background: var(--card);
    color: var(--foreground);
    font-size: 12px;
    cursor: pointer;
  }
  .starter-alt-btn:hover {
    border-color: var(--primary);
  }
  .template {
    display: inline-flex;
    align-items: center;
    gap: 7px;
    padding: 7px 14px;
    border: 1px solid
      color-mix(in oklab, var(--template-color) 45%, var(--border));
    border-radius: 999px;
    background: var(--card);
    color: var(--muted-foreground);
    font-size: 13px;
    font-weight: 550;
    white-space: nowrap;
    transition:
      color 120ms ease,
      border-color 120ms ease,
      background-color 120ms ease;
  }
  .template :global(svg) {
    flex: none;
    color: var(--template-color);
  }
  .template:hover {
    color: var(--foreground);
    border-color: var(--template-color);
  }
  .template:focus-visible {
    outline: 2px solid var(--template-color);
    outline-offset: 2px;
  }
  .template[aria-pressed="true"] {
    color: #fff;
    border-color: var(--template-color);
    background: var(--template-color);
  }
  .template[aria-pressed="true"] :global(svg) {
    color: currentColor;
  }
  @media (prefers-reduced-motion: reduce) {
    .expansion {
      width: 100%;
      text-align: center;
      color: var(--muted-foreground);
      font-size: 13px;
    }
    .template {
      transition: none;
    }
  }
</style>
