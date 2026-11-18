<script lang="ts">
  import { Rocket, FolderOpen, Briefcase, House } from "@lucide/svelte";
  import type {
    CreationTemplate,
    CreationTemplateIcon,
    CreationTemplateId,
    TemplateSelection,
  } from "#lib/wizard/creationTemplates.js";
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
    cards,
  }: {
    templates: { template: CreationTemplate; selection: TemplateSelection }[];
    /** The profiles that are on. */
    live: readonly CreationTemplateId[];
    ontoggle: (id: CreationTemplateId) => void;
    standardStart: boolean;
    expansion: Record<CreationTemplateId, { expansion: readonly string[] }>;
    cards: CreationGoal[];
  } = $props();

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
        {@const later = [
          ...new Set([
            ...expansion[template.id].expansion,
            ...selection.skipped,
          ]),
        ]}
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
