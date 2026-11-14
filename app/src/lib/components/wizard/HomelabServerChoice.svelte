<!--
  HomelabServerChoice: the first sub-step of the Server step for a person who
  already has servers. Their homelab is everything they run; here they pick the
  one server that takes the new use cases, or a new server, which the rest of
  the step then sets up. A connected server without a StackKit can also join
  one of their running deployments as another server of it.
-->
<script lang="ts">
  import { Check, Layers, Plus, Server } from "@lucide/svelte";
  import type { HomelabServerSelection } from "#lib/creation/homelab-servers.js";
  import LoadingNote from "#lib/components/ui/LoadingNote.svelte";
  import { tr } from "#lib/i18n.svelte.js";

  let { homelab }: { homelab: HomelabServerSelection } = $props();

  const NOTE_DEFAULT = "wizard.server.homelab.disconnected";
  const NOTE: Record<string, string> = {
    disconnected: "wizard.server.homelab.disconnected",
    leased: "wizard.server.homelab.leased",
    restricted: "wizard.server.homelab.restricted",
  };

  const chosen = $derived(
    homelab.servers.find((server) => server.id === homelab.selected),
  );
  // Only a connected server without a StackKit can join a deployment.
  const joinOffered = $derived(
    Boolean(chosen) &&
      chosen?.state === "ready" &&
      !chosen?.deploymentId &&
      homelab.joinTargets.length > 0,
  );
</script>

<section
  class="homelab-region"
  data-substep
  aria-labelledby="homelab-servers-heading"
  data-testid="homelab-servers"
>
  <div class="homelab-heading">
    <h3 id="homelab-servers-heading">{tr("wizard.server.homelab.title")}</h3>
    <p>{tr("wizard.server.homelab.lead")}</p>
  </div>

  {#if homelab.loading}
    <LoadingNote label={tr("wizard.server.homelab.loading")} />
  {:else}
    {#if homelab.notice}
      <p class="read-failed" role="status" data-testid="use-case-preferred-skipped">
        {homelab.notice}
      </p>
    {/if}
    {#if homelab.failed}
      <p class="read-failed" role="status" data-testid="homelab-servers-failed">
        {tr("wizard.server.homelab.failed")}
        <button type="button" onclick={() => homelab.retry()}
          >{tr("common.retry")}</button
        >
      </p>
    {/if}
    <div class="server-grid" role="radiogroup" aria-labelledby="homelab-servers-heading">
      {#each homelab.servers as server (server.id)}
        {@const selected = homelab.selected === server.id}
        {@const ready = server.state === "ready"}
        <label
          class="server-card"
          class:selected
          class:unavailable={!ready}
          data-testid="homelab-server"
          data-server-id={server.id}
          data-state={server.state}
          data-has-stackkit={Boolean(server.deploymentId)}
        >
          <input
            type="radio"
            name="homelab-server"
            class="sr-only"
            value={server.id}
            checked={selected}
            disabled={!ready}
            onchange={() => homelab.select(server.id)}
          />
          <span class="server-icon"><Server size={20} strokeWidth={1.6} aria-hidden="true" /></span>
          <span class="server-copy">
            <strong>{server.name}</strong>
            <small
              >{ready
                ? server.deploymentId
                  ? server.stackkit
                    ? tr("wizard.server.homelab.runs", { stackkit: server.stackkit })
                    : tr("wizard.server.homelab.runsStackkit")
                  : tr("wizard.server.homelab.firstStackkit")
                : tr(NOTE[server.state] ?? NOTE_DEFAULT)}</small
            >
          </span>
          <span class="server-mark" aria-hidden="true"
            >{#if selected}<Check size={16} />{/if}</span
          >
        </label>
      {/each}
      <label
        class="server-card new"
        class:selected={!homelab.selected}
        data-testid="homelab-server-new"
      >
        <input
          type="radio"
          name="homelab-server"
          class="sr-only"
          value=""
          checked={!homelab.selected}
          onchange={() => homelab.select("")}
        />
        <span class="server-icon"><Plus size={20} strokeWidth={1.6} aria-hidden="true" /></span>
        <span class="server-copy">
          <strong>{tr("wizard.server.homelab.new")}</strong>
          <small>{tr("wizard.server.homelab.newBody")}</small>
        </span>
        <span class="server-mark" aria-hidden="true"
          >{#if !homelab.selected}<Check size={16} />{/if}</span
        >
      </label>
    </div>
    {#if joinOffered && chosen}
      <div
        class="use-region"
        role="radiogroup"
        aria-labelledby="homelab-server-use-heading"
        data-testid="homelab-server-use"
      >
        <h4 id="homelab-server-use-heading">
          {tr("wizard.server.homelab.useTitle", { server: chosen.name })}
        </h4>
        <div class="server-grid">
          <label class="server-card" class:selected={!homelab.joinTarget} data-testid="homelab-server-use-new">
            <input
              type="radio"
              name="homelab-server-use"
              class="sr-only"
              value=""
              checked={!homelab.joinTarget}
              onchange={() => homelab.selectJoin("")}
            />
            <span class="server-icon"><Plus size={20} strokeWidth={1.6} aria-hidden="true" /></span>
            <span class="server-copy">
              <strong>{tr("wizard.server.homelab.useNew")}</strong>
              <small>{tr("wizard.server.homelab.firstStackkit")}</small>
            </span>
            <span class="server-mark" aria-hidden="true"
              >{#if !homelab.joinTarget}<Check size={16} />{/if}</span
            >
          </label>
          {#each homelab.joinTargets as deployment (deployment.id)}
            {@const selected = homelab.joinTarget === deployment.id}
            <label
              class="server-card"
              class:selected
              data-testid="homelab-server-use-join"
              data-deployment-id={deployment.id}
            >
              <input
                type="radio"
                name="homelab-server-use"
                class="sr-only"
                value={deployment.id}
                checked={selected}
                onchange={() => homelab.selectJoin(deployment.id)}
              />
              <span class="server-icon"><Layers size={20} strokeWidth={1.6} aria-hidden="true" /></span>
              <span class="server-copy">
                <strong>{tr("wizard.server.homelab.useJoin", { deployment: deployment.name })}</strong>
                <small>{tr("wizard.server.homelab.useJoinBody", { deployment: deployment.name })}</small>
              </span>
              <span class="server-mark" aria-hidden="true"
                >{#if selected}<Check size={16} />{/if}</span
              >
            </label>
          {/each}
        </div>
      </div>
    {/if}
  {/if}
</section>

<style>
  .homelab-region {
    --substep-top: 2px;
  }
  .homelab-heading {
    margin-bottom: 18px;
  }
  .homelab-heading h3 {
    font-size: 20px;
    font-weight: 550;
    letter-spacing: -0.025em;
  }
  .homelab-heading p {
    margin-top: 6px;
    color: var(--muted-foreground);
    font-size: 13px;
    line-height: 1.6;
  }
  .server-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(min(100%, 240px), 1fr));
    gap: 14px;
  }
  .server-card {
    display: grid;
    grid-template-columns: auto minmax(0, 1fr) 18px;
    align-items: center;
    gap: 14px;
    padding: 16px 18px;
    border: 1px solid var(--border);
    border-radius: var(--radius-panel, 18px);
    background: var(--card);
    cursor: pointer;
    transition:
      border-color 180ms,
      background 180ms;
  }
  .server-card:hover:not(.unavailable) {
    border-color: color-mix(in oklch, var(--primary) 65%, var(--border));
  }
  .server-card.selected {
    border-color: var(--primary);
    background: color-mix(in oklch, var(--primary) 5%, var(--card));
  }
  .server-card:has(input:focus-visible) {
    outline: 2px solid var(--ring);
    outline-offset: 2px;
  }
  .server-card.unavailable {
    cursor: not-allowed;
    color: var(--muted-foreground);
  }
  .server-card.new {
    border-style: dashed;
  }
  .server-card.new.selected {
    border-style: solid;
  }
  .server-icon {
    display: grid;
    place-items: center;
    width: 38px;
    height: 38px;
    border-radius: 12px;
    color: var(--primary);
    background: color-mix(in oklch, var(--primary) 9%, transparent);
  }
  .unavailable .server-icon {
    color: var(--muted-foreground);
    background: var(--muted);
  }
  .server-copy {
    min-width: 0;
  }
  .server-copy strong {
    display: block;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: 15px;
    font-weight: 550;
    letter-spacing: -0.01em;
  }
  .server-copy small {
    display: block;
    margin-top: 3px;
    color: var(--muted-foreground);
    font-size: 12px;
    line-height: 1.5;
  }
  .server-mark {
    color: var(--primary);
  }
  .use-region {
    margin-top: 22px;
  }
  .use-region h4 {
    margin-bottom: 12px;
    font-size: 15px;
    font-weight: 550;
    letter-spacing: -0.01em;
  }
  .read-failed {
    margin-bottom: 14px;
    color: var(--muted-foreground);
    font-size: 13px;
  }
  .read-failed button {
    margin-inline-start: 8px;
    color: var(--primary);
    text-decoration: underline;
    text-underline-offset: 2px;
  }
</style>
