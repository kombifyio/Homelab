<script lang="ts">
  /**
   * The Companion tab of the settings (Windows client only): the desktop
   * Companion's settings as sections of rows, drawn exactly like the other
   * tabs, from the product's own settings components. The data is the
   * Companion's (SETTINGS-CONTRACT 2.2 over the shell bridge); the look is
   * Techstack's. Labels only, no explanation text; an unavailable row is
   * dimmed and carries its reason for assistive technology only.
   */
  import { onMount } from "svelte";
  import { SettingsRow, SettingsSection } from "@kombiverselabs/ui/settings";
  import Button from "#lib/components/ui/Button.svelte";
  import Switch from "#lib/components/ui/Switch.svelte";
  import CompanionChordButton from "#lib/components/settings/CompanionChordButton.svelte";
  import {
    createCompanionSettingsController,
    type CompanionSettingsState,
  } from "#lib/settings/companion/controller.js";
  import type { CompanionSettingsAdapter } from "#lib/settings/companion/contract.js";
  import { companionSettingsLabels } from "#lib/settings/companion/labels.js";
  import { companionSettingsView } from "#lib/settings/companion/model.js";

  interface Props {
    adapter: CompanionSettingsAdapter;
  }

  let { adapter }: Props = $props();

  // One controller for the tab: it reads and watches while the tab is mounted.
  // The adapter is fixed for the tab's life (the page creates it once).
  // svelte-ignore state_referenced_locally
  const controller = createCompanionSettingsController(adapter);
  let state = $state<CompanionSettingsState | null>(null);
  const current = $derived(state ?? controller.getState());

  onMount(() => {
    const unsubscribe = controller.subscribe((next) => (state = next));
    state = controller.getState();
    const release = controller.retain();
    return () => {
      unsubscribe();
      release();
    };
  });

  const labels = $derived(companionSettingsLabels());
  const view = $derived(companionSettingsView(current, labels));
</script>

<div class="space-y-6" data-testid="companion-settings-page" data-state={view.state}>
  {#each view.groups as group (group.id)}
    <SettingsSection id={`companion-${group.id}`} title={group.title}>
      {#each group.rows as row (row.key)}
        {#if row.kind === "destination"}
          <SettingsRow label={row.label}>
            <Button
              variant="secondary"
              ariaLabel={`${labels.actions.open} · ${row.name}`}
              onclick={() => controller.openLink(row.url)}
            >
              {labels.actions.open}
            </Button>
          </SettingsRow>
        {:else}
          <SettingsRow
            label={row.label}
            for={row.kind === "switch" ||
            row.kind === "select" ||
            row.kind === "chord"
              ? `companion-${row.key}`
              : undefined}
          >
            {#if row.kind === "switch"}
              <Switch
                id={`companion-${row.key}`}
                testId={`companion-${row.key}`}
                checked={row.value}
                label={row.name}
                disabled={row.unavailable || row.pending}
                onchange={(next) => void controller.write(row.key, next)}
                onText={labels.values.on}
                offText={labels.values.off}
              />
            {:else if row.kind === "choice"}
              <div
                class="flex flex-wrap gap-1"
                role="radiogroup"
                aria-label={row.name}
                data-testid={`companion-${row.key}`}
              >
                {#each row.options as option (option.value)}
                  <button
                    type="button"
                    role="radio"
                    aria-checked={row.value === option.value}
                    disabled={row.unavailable || row.pending}
                    data-kx={row.value === option.value
                      ? "control selection"
                      : "control"}
                    class="rounded-lg px-3 py-1.5 text-sm disabled:cursor-not-allowed disabled:opacity-60"
                    onclick={() =>
                      void controller.write(row.key, option.value)}
                  >
                    {option.label}
                  </button>
                {/each}
              </div>
            {:else if row.kind === "select"}
              <select
                id={`companion-${row.key}`}
                class="rounded-lg border border-border bg-card px-3 py-1.5 text-sm text-foreground disabled:cursor-not-allowed disabled:opacity-60"
                aria-label={row.name}
                disabled={row.unavailable || row.pending}
                value={row.value}
                onchange={(event) =>
                  void controller.write(row.key, event.currentTarget.value)}
                data-testid={`companion-${row.key}`}
              >
                {#each row.options as option (option.value)}
                  <option value={option.value}>{option.label}</option>
                {/each}
              </select>
            {:else if row.kind === "chord"}
              <CompanionChordButton
                id={`companion-${row.key}`}
                value={row.value}
                label={row.name}
                emptyLabel={labels.values.chordNone}
                clearLabel={labels.actions.removeShortcut}
                clearable={row.clearable}
                disabled={row.unavailable || row.pending}
                recordingLabel={labels.status.recording}
                describedBy={row.refusal ? `companion-${row.key}-refusal` : undefined}
                names={labels.keys}
                onchange={(chord) => void controller.writeChord(row.key, chord)}
                onrefuse={() =>
                  controller.refuse(row.key, { error: "shortcut_invalid" })}
              />
            {:else if row.kind === "folder"}
              <div class="flex flex-wrap items-center gap-2">
                <span
                  id={`companion-${row.key}`}
                  class="break-all text-sm text-muted-foreground"
                >
                  {row.value}
                </span>
                <Button
                  variant="secondary"
                  ariaLabel={`${labels.actions.choose} · ${row.name}`}
                  disabled={row.unavailable || row.pending}
                  onclick={() => void controller.chooseFolder(row.key)}
                >
                  {labels.actions.choose}
                </Button>
              </div>
            {:else if row.kind === "account"}
              <span
                id={`companion-${row.key}`}
                class="text-sm {row.signedIn
                  ? 'text-foreground'
                  : 'text-muted-foreground'}"
              >
                {row.value}
              </span>
            {/if}
            {#if row.refusal}
              <span
                id={`companion-${row.key}-refusal`}
                class="ms-3 text-xs text-destructive"
                role="alert"
              >
                {row.refusal}
              </span>
            {/if}
            <!-- One live region per row, kept in place: it speaks the write in flight. -->
            <span class="sr-only" role="status">
              {row.pending ? labels.status.saving : ""}
            </span>
          </SettingsRow>
        {/if}
      {/each}
    </SettingsSection>
  {/each}
</div>
