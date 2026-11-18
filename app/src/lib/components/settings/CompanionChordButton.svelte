<script lang="ts">
  /**
   * The shortcut control of a Companion setting: a button that shows the chord
   * and, once pressed, records the next one (every key held, complete when the
   * last is released). Plain settings controls: `data-kx="control"` buttons
   * like the segmented choices of the Appearance tab.
   */
  import { onDestroy } from "svelte";
  import {
    chordCodeForPointerButton,
    chordParts,
    createChordCapture,
    type ChordCaptureState,
    type ChordKeyNames,
  } from "#lib/settings/companion/chord.js";

  interface Props {
    id: string;
    value: string;
    /** The accessible name: the row label, plus the reason when unavailable. */
    label: string;
    emptyLabel: string;
    clearLabel: string;
    clearable: boolean;
    disabled: boolean;
    /** Spoken while a shortcut is being recorded. */
    recordingLabel: string;
    /** Id of the row's refusal text, so a conflict is read with the control. */
    describedBy?: string;
    names: ChordKeyNames;
    onchange: (chord: string) => void;
    onrefuse: () => void;
  }

  let {
    id,
    value,
    label,
    emptyLabel,
    clearLabel,
    clearable,
    disabled,
    recordingLabel,
    describedBy,
    names,
    onchange,
    onrefuse,
  }: Props = $props();

  const SINGLE_MODIFIER = new Set(["ctrl", "alt", "shift", "win"]);

  let capture = $state<ChordCaptureState>({ recording: false, held: [] });
  const recorder = createChordCapture({
    // A single modifier on its own is no shortcut: it is dropped, not written. (Two modifiers such as
    // ctrl+win are the default dictation chord and are written.)
    onCommit: (chord) => {
      if (SINGLE_MODIFIER.has(chord)) return;
      onchange(chord);
    },
    onRefuse: () => onrefuse(),
  });
  const unsubscribe = recorder.subscribe((next) => (capture = next));
  onDestroy(() => {
    unsubscribe();
    recorder.cancel();
  });

  const shown = $derived(capture.recording ? capture.held.join("+") : value);
  const parts = $derived(shown ? chordParts(shown, names) : []);
  // The accessible name carries the chord: the label alone would hide what the button shows.
  const name = $derived(`${label}: ${parts.length ? parts.join(" + ") : emptyLabel}`);

  function keydown(event: KeyboardEvent) {
    if (!capture.recording) return;
    // Tab leaves the control (focus moves, recording ends) and Escape cancels: neither is ever recorded,
    // so the keyboard is never trapped and no chord is committed by a cancel.
    if (event.code === "Tab") {
      recorder.cancel();
      return;
    }
    if (event.code === "Escape") {
      event.preventDefault();
      recorder.cancel();
      return;
    }
    if (recorder.down(event.code)) event.preventDefault();
  }
  function keyup(event: KeyboardEvent) {
    if (recorder.up(event.code)) event.preventDefault();
  }
  function pointerdown(event: PointerEvent) {
    const code = chordCodeForPointerButton(event.button);
    if (code && recorder.down(code)) event.preventDefault();
  }
  function pointerup(event: PointerEvent) {
    const code = chordCodeForPointerButton(event.button);
    if (code && recorder.up(code)) event.preventDefault();
  }
</script>

<div class="flex flex-wrap items-center gap-2">
  <button
    {id}
    type="button"
    {disabled}
    aria-label={name}
    aria-describedby={describedBy}
    data-kx={capture.recording ? "control selection" : "control"}
    class="rounded-lg px-3 py-1.5 text-sm disabled:cursor-not-allowed disabled:opacity-60"
    onclick={() => (capture.recording ? recorder.cancel() : recorder.start())}
    onblur={() => recorder.cancel()}
    onkeydown={keydown}
    onkeyup={keyup}
    onpointerdown={pointerdown}
    onpointerup={pointerup}
    oncontextmenu={(event) => capture.recording && event.preventDefault()}
  >
    {#if parts.length}
      {parts.join(" + ")}
    {:else}
      {emptyLabel}
    {/if}
  </button>
  {#if clearable && value && !capture.recording}
    <button
      type="button"
      {disabled}
      data-kx="control"
      class="rounded-lg px-2 py-1 text-xs text-muted-foreground disabled:cursor-not-allowed disabled:opacity-60"
      onclick={() => onchange("")}
    >
      {clearLabel}
    </button>
  {/if}
  <span class="sr-only" role="status">{capture.recording ? recordingLabel : ""}</span>
</div>
