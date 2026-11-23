<script lang="ts">
  /**
   * The Companion when the person chose a local answer (workspace
   * `platform/standards/local-ai-runtimes.v1.md` rule 9, `local-answer.svelte.ts`): it stands in
   * for the kombify Companion (`CompanionHost`) and answers on the runtime the person chose, this
   * PC's Ollama or the Ollama of their homelab server. The Windows shell runs each turn on that
   * runtime (`ClientWindow.LocalAi.cs`); the page only sends the conversation, so a turn runs
   * where the person chose and nowhere else. A runtime that cannot answer is a reason line,
   * never a turn on kombify AI.
   *
   * The model reads the page the person is on (the same view context the kombify Companion
   * gets) and answers questions about it. The homelab agent's tools, which change servers and
   * apps, run in kombify's governed runtime; the panel says so in one line. Every answer names its
   * source: the runtime and the model.
   *
   * A small surface on purpose, an agent design (2026-11-23): launcher, conversation, composer.
   * Screens hand it a question through `companion.ask` exactly as they do the kombify Companion.
   */
  import { onMount, tick } from "svelte";
  import { Cpu, Plus, Send, Square, X } from "@lucide/svelte";
  import { tr } from "#lib/i18n.svelte.js";
  import { registerCompanion } from "#lib/companion/ask.svelte.js";
  import { localAnswer } from "#lib/ai/local-answer.svelte.js";
  import { localAiReasonText, runtimeName } from "#lib/ai/local-ai-names.js";
  import { refreshAiSetup } from "#lib/ai/ai-setup-state.js";
  import { currentViewContext } from "#lib/stores/viewContext.svelte.js";
  import {
    chatLocalAi,
    type LocalChatMessage,
    type LocalChatRefusal,
  } from "#lib/client/local-ai-bridge.js";

  interface Turn {
    id: number;
    role: "user" | "assistant";
    text: string;
    /** "Ollama (this PC) · qwen2.5:3b" on an answer. */
    source?: string;
    /** Why this answer did not come. */
    refusal?: LocalChatRefusal;
    pending?: boolean;
  }

  // The history a turn carries: the newest turns, within what the shell takes in one message.
  const HISTORY_TURNS = 24;
  const HISTORY_CHARS = 60_000;

  let open = $state(false);
  let turns = $state<Turn[]>([]);
  let draft = $state("");
  let running = $state<AbortController | null>(null);
  let list = $state<HTMLDivElement | null>(null);
  let input = $state<HTMLTextAreaElement | null>(null);
  let nextId = 0;

  const view = $derived(localAnswer.view);
  const name = $derived(view ? runtimeName(view) : "");
  const sourceLine = $derived(
    view
      ? tr("localCompanion.source", { name, model: view.model || "…" })
      : "",
  );
  const blocked = $derived(
    view?.reason ? localAiReasonText(view.reason, name, view.model) : "",
  );

  function systemPrompt(): string {
    const page = currentViewContext();
    const lines = [
      "You are the kombify Companion inside Homelab Runtime, the app that runs the person's homelab: their servers, stacks and self-hosted apps.",
      "You run on the person's own model, so you answer from what you know and from the page below. You cannot change servers or apps from here; when the person wants a change, tell them the steps in Homelab Runtime or that the kombify Companion can carry it out.",
      "Answer briefly and concretely, in the language of the person's question.",
    ];
    if (page) {
      lines.push("", `The person is on the page "${page.title}" (${page.route}).`);
      if (page.state) lines.push(`Page state: ${page.state}.`);
      if (page.subject) lines.push(`Shown object: ${JSON.stringify(page.subject)}.`);
      if (page.facts && Object.keys(page.facts).length > 0)
        lines.push(`Facts on the page: ${JSON.stringify(page.facts)}.`);
    }
    return lines.join("\n");
  }

  function history(): LocalChatMessage[] {
    const done = turns.filter((turn) => !turn.pending && !turn.refusal && turn.text);
    const messages: LocalChatMessage[] = [];
    let chars = 0;
    for (const turn of done.slice(-HISTORY_TURNS).reverse()) {
      chars += turn.text.length;
      if (chars > HISTORY_CHARS) break;
      messages.unshift({ role: turn.role, content: turn.text });
    }
    return messages;
  }

  async function scrollDown() {
    await tick();
    list?.scrollTo({ top: list.scrollHeight });
  }

  async function send(prompt: string, fresh = false) {
    const text = prompt.trim();
    if (!text || running) return;
    if (fresh) turns = [];
    const messages: LocalChatMessage[] = [
      { role: "system", content: systemPrompt() },
      ...history(),
      { role: "user", content: text },
    ];
    turns.push({ id: nextId++, role: "user", text });
    turns.push({ id: nextId++, role: "assistant", text: "", pending: true });
    // The reactive entry, so each piece repaints the answer.
    const answer = turns[turns.length - 1] as Turn;
    draft = "";
    const controller = new AbortController();
    running = controller;
    void scrollDown();
    try {
      const outcome = await chatLocalAi(
        messages,
        (piece) => {
          answer.text += piece;
          void scrollDown();
        },
        controller.signal,
      );
      const answeredName = runtimeName(outcome);
      answer.source = tr("localCompanion.answeredBy", {
        name: answeredName,
        model: outcome.model,
      });
      if (!outcome.answered) answer.refusal = outcome.reason;
    } catch {
      answer.refusal = "failed";
    } finally {
      answer.pending = false;
      running = null;
      void scrollDown();
      // The shell probed the runtime for this turn; the setup reflects what answered.
      void refreshAiSetup(true);
    }
  }

  function refusalText(turn: Turn): string {
    if (!turn.refusal) return "";
    return localAiReasonText(turn.refusal, name, view?.model ?? "");
  }

  function show(next: boolean) {
    open = next;
    if (next) {
      void refreshAiSetup(true);
      void tick().then(() => input?.focus());
    }
  }

  onMount(() =>
    registerCompanion({
      submit: ({ prompt, freshSession }) => {
        show(true);
        void send(prompt, freshSession === true);
      },
    }),
  );
</script>

<div class="local-companion" data-testid="local-companion">
  {#if open}
    <section
      class="flex h-[min(36rem,calc(100vh-7rem))] w-[min(24rem,calc(100vw-2rem))] flex-col overflow-hidden rounded-2xl border border-border bg-card text-foreground shadow-2xl"
      aria-label={tr("localCompanion.title")}
      data-testid="local-companion-panel"
    >
      <header class="flex items-start gap-3 border-b border-border px-4 py-3">
        <span
          class="mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-full bg-primary/10 text-primary"
          aria-hidden="true"
        >
          <Cpu class="size-4" />
        </span>
        <div class="min-w-0 flex-1">
          <h2 class="text-sm font-semibold">{tr("localCompanion.title")}</h2>
          <p
            class="truncate text-xs text-muted-foreground"
            data-testid="local-companion-source"
            title={sourceLine}
          >
            {sourceLine}
          </p>
        </div>
        <button
          type="button"
          class="rounded-lg p-1.5 text-muted-foreground hover:bg-muted hover:text-foreground disabled:opacity-50"
          aria-label={tr("localCompanion.new")}
          title={tr("localCompanion.new")}
          disabled={running !== null || turns.length === 0}
          onclick={() => (turns = [])}
        >
          <Plus class="size-4" />
        </button>
        <button
          type="button"
          class="rounded-lg p-1.5 text-muted-foreground hover:bg-muted hover:text-foreground"
          aria-label={tr("localCompanion.close")}
          onclick={() => show(false)}
        >
          <X class="size-4" />
        </button>
      </header>

      <div
        bind:this={list}
        class="flex-1 space-y-3 overflow-y-auto px-4 py-3"
        aria-live="polite"
      >
        {#if turns.length === 0}
          <p class="text-sm text-muted-foreground">{tr("localCompanion.empty")}</p>
          <p class="text-xs text-muted-foreground">{tr("settings.aiProviders.answer.scope")}</p>
        {/if}
        {#each turns as turn (turn.id)}
          {#if turn.role === "user"}
            <div class="flex justify-end">
              <p
                class="max-w-[85%] whitespace-pre-wrap rounded-2xl rounded-ee-sm bg-primary px-3 py-2 text-sm text-primary-foreground"
              >
                {turn.text}
              </p>
            </div>
          {:else}
            <div class="max-w-[92%] space-y-1" data-testid="local-companion-answer">
              {#if turn.text}
                <p class="whitespace-pre-wrap text-sm leading-relaxed">{turn.text}</p>
              {:else if turn.pending}
                <p class="text-sm text-muted-foreground">{tr("localCompanion.thinking")}</p>
              {/if}
              {#if turn.refusal}
                <p class="text-sm text-destructive" role="status">
                  {refusalText(turn)}
                  {#if turn.refusal !== "cancelled"}
                    {tr("localCompanion.notSent")}
                    <a class="underline" href="/settings?tab=ai-providers"
                      >{tr("localCompanion.change")}</a
                    >
                  {/if}
                </p>
              {/if}
              {#if turn.source && !turn.pending && !turn.refusal}
                <p class="text-[11px] text-muted-foreground">{turn.source}</p>
              {/if}
            </div>
          {/if}
        {/each}
      </div>

      {#if blocked}
        <p
          class="border-t border-border px-4 py-2 text-xs text-destructive"
          role="status"
          data-testid="local-companion-blocked"
        >
          {blocked}
          {tr("localCompanion.notSent")}
          <a class="underline" href="/settings?tab=ai-providers"
            >{tr("localCompanion.change")}</a
          >
        </p>
      {/if}

      <form
        class="flex items-end gap-2 border-t border-border px-3 py-3"
        onsubmit={(event) => {
          event.preventDefault();
          void send(draft);
        }}
      >
        <textarea
          bind:this={input}
          bind:value={draft}
          rows="2"
          class="min-h-10 flex-1 resize-none rounded-xl border border-border bg-background px-3 py-2 text-sm text-foreground"
          placeholder={tr("localCompanion.placeholder")}
          aria-label={tr("localCompanion.placeholder")}
          data-testid="local-companion-input"
          onkeydown={(event) => {
            if (event.key === "Enter" && !event.shiftKey) {
              event.preventDefault();
              void send(draft);
            }
          }}
        ></textarea>
        {#if running}
          <button
            type="button"
            class="flex size-9 items-center justify-center rounded-full bg-muted text-foreground"
            aria-label={tr("localCompanion.stop")}
            onclick={() => running?.abort()}
          >
            <Square class="size-4" />
          </button>
        {:else}
          <button
            type="submit"
            class="flex size-9 items-center justify-center rounded-full bg-primary text-primary-foreground disabled:opacity-50"
            aria-label={tr("localCompanion.send")}
            disabled={!draft.trim()}
            data-testid="local-companion-send"
          >
            <Send class="size-4" />
          </button>
        {/if}
      </form>
    </section>
  {/if}

  <button
    type="button"
    class="flex items-center gap-2 rounded-full border border-border bg-card px-4 py-2.5 text-sm font-medium text-foreground shadow-lg hover:bg-muted"
    aria-expanded={open}
    aria-label={open ? tr("localCompanion.close") : tr("localCompanion.open")}
    data-testid="local-companion-launcher"
    onclick={() => show(!open)}
  >
    <Cpu class="size-4 text-primary" aria-hidden="true" />
    {tr("localCompanion.launcher")}
  </button>
</div>

<style>
  .local-companion {
    position: fixed;
    inset-inline-end: 1.25rem;
    bottom: 1.25rem;
    z-index: 60;
    display: flex;
    flex-direction: column;
    align-items: flex-end;
    gap: 0.75rem;
  }
</style>
