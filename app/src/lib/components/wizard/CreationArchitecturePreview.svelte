<script lang="ts">
  import type { StackConfig } from "#lib/wizard/types.js";
  import { selectedCanonicalUseCasesFromGoals } from "#lib/wizard/index.js";
  import { useCaseTitle } from "#lib/wizard/creationGoalText.js";
  import { tr } from "#lib/i18n.svelte.js";
  import type { HomelabServerSelection } from "#lib/creation/homelab-servers.js";

  interface Props {
    config: StackConfig;
    recommendedStackKit?: string;
    homelab?: HomelabServerSelection;
  }

  let { config, recommendedStackKit, homelab }: Props = $props();
  let expanded = $state(false);

  const selectedGoals = $derived(
    selectedCanonicalUseCasesFromGoals(config.goals).map((goal) => ({
      id: goal,
      title: useCaseTitle(goal),
    })),
  );
  const kitLabel = $derived(
    (recommendedStackKit || config.kit || "StackKit")
      .split("-")
      .filter(Boolean)
      .map((part) => part[0]?.toUpperCase() + part.slice(1))
      .join(" "),
  );
  const serverLabel = $derived(
    homelab?.selected
      ? (homelab.servers.find((server) => server.id === homelab.selected)?.name ??
        tr("wizard.server.homelab.title"))
      : config.serverProvisioning.mode === "kombify-cloud"
        ? tr("wizard.summary.server.managed")
        : config.serverProvisioning.mode === "hypervisor"
          ? tr("wizard.summary.server.hypervisor")
          : config.serverProvisioning.mode === "connect-remote"
            ? tr("wizard.summary.server.connectRemote")
            : tr("wizard.summary.server.installCommand"),
  );
  const accessLabel = $derived(
    config.network.accessMode === "home" || config.network.accessMode === "anywhere"
      ? tr(`wizard.access.${config.network.accessMode}.label`)
      : tr("wizard.summary.users.solo"),
  );
</script>

<section
  data-kx="plate"
  class="mt-5 rounded-2xl border border-primary/20 bg-primary/5 p-4"
  data-testid="creation-architecture-preview"
>
  <label class="flex cursor-pointer items-start gap-3">
    <input
      type="checkbox"
      class="mt-1 h-4 w-4 accent-[var(--primary)]"
      bind:checked={expanded}
      aria-describedby="creation-architecture-preview-description"
    />
    <span>
      <span class="block font-semibold text-foreground">
        {tr("wizard.hints.why")}
      </span>
      <span
        id="creation-architecture-preview-description"
        class="mt-1 block text-sm text-muted-foreground"
      >
        {tr("wizard.hints.why")}
      </span>
    </span>
  </label>

  {#if expanded}
    <div class="mt-4 border-t border-primary/15 pt-4">
      <div class="architecture-flow" aria-label={tr("wizard.hints.title")}>
        <div class="architecture-node architecture-root">
          <span class="node-kicker">{tr("wizard.hints.title")}</span>
          <strong>{config.name || tr("wizard.title")}</strong>
        </div>
        <div class="architecture-branch" aria-hidden="true"></div>
        <div class="architecture-grid">
          <div class="architecture-node">
            <span class="node-kicker">{tr("wizard.summary.server.installCommand")}</span>
            <strong>{serverLabel}</strong>
          </div>
          <div class="architecture-node">
            <span class="node-kicker">{tr("wizard.hints.title")}</span>
            <strong>{kitLabel}</strong>
          </div>
          <div class="architecture-node">
            <span class="node-kicker">{tr("wizard.access.title")}</span>
            <strong>{accessLabel}</strong>
          </div>
        </div>
      </div>

      {#if selectedGoals.length > 0}
        <div class="mt-4 flex flex-wrap gap-2" aria-label={tr("wizard.goals.configure.list")}>
          {#each selectedGoals as goal (goal.id)}
            <span class="rounded-full border border-border bg-card px-3 py-1.5 text-xs text-foreground">
              {goal.title}
            </span>
          {/each}
        </div>
      {/if}
    </div>
  {/if}
</section>
