/**
 * Navigation through one creation process, without a framework: the current
 * step, how far the person got, and the moves between steps. It follows the
 * store contract, so a frame reads it like any other store. What a move means
 * on screen (address, focus, the list of what is missing) belongs to the frame.
 */
import { creationStepOrder, type CreationProcess } from "./processes";
import { creationStepNumber, type CreationStepKey } from "./steps";

export interface CreationSessionState {
  process: CreationProcess;
  current: CreationStepKey;
  furthest: CreationStepKey;
}

/**
 * `moved`: the step is now current. `blocked`: the move stopped on this step,
 * which is not complete, and it is now current. `stay`: nothing to do.
 */
export type CreationMove =
  { kind: "moved" | "blocked"; step: CreationStepKey } | { kind: "stay" };

export interface CreationSession {
  subscribe(run: (state: CreationSessionState) => void): () => void;
  /** Steps of this process that can be opened, in order. */
  activeSteps(): CreationStepKey[];
  /** The active steps already reached, without the current one. */
  reachableSteps(): CreationStepKey[];
  lastStep(): CreationStepKey;
  /** The first active step before `target` that is not complete. */
  firstIncompleteBefore(target: CreationStepKey): CreationStepKey | undefined;
  next(): CreationMove;
  prev(): CreationMove;
  /** A reached step. Forward, every step on the way must still be complete. */
  goTo(target: CreationStepKey): CreationMove;
  /** Opens a step directly, for a move that needs no check. */
  open(target: CreationStepKey): void;
  setProcess(process: CreationProcess): void;
}

const activeOf = (process: CreationProcess) =>
  creationStepOrder(process).filter((key) => !process.inactive[key]);

export function createCreationSession(init: {
  process: CreationProcess;
  current?: CreationStepKey;
  furthest?: CreationStepKey;
  /** Asked at the moment of a move, so it may read live answers. */
  isComplete: (key: CreationStepKey) => boolean;
}): CreationSession {
  const first = activeOf(init.process)[0] ?? "goals";
  const start = init.current ?? first;
  let state: CreationSessionState = {
    process: init.process,
    current: start,
    furthest: start,
  };
  // Places in the process's own order; a process may change while it runs.
  const position = (key: CreationStepKey) =>
    creationStepNumber(key, creationStepOrder(state.process));
  if (init.furthest && position(init.furthest) > position(start))
    state.furthest = init.furthest;
  const listeners = new Set<(state: CreationSessionState) => void>();

  function commit(next: Partial<CreationSessionState>) {
    state = { ...state, ...next };
    for (const listener of listeners) listener(state);
  }

  function open(target: CreationStepKey) {
    commit({
      current: target,
      furthest:
        position(target) > position(state.furthest) ? target : state.furthest,
    });
  }

  const activeSteps = () => activeOf(state.process);

  return {
    subscribe(run) {
      listeners.add(run);
      run(state);
      return () => listeners.delete(run);
    },
    activeSteps,
    reachableSteps: () =>
      activeSteps().filter(
        (key) =>
          position(key) <= position(state.furthest) && key !== state.current,
      ),
    lastStep: () => activeSteps().at(-1) ?? first,
    firstIncompleteBefore: (target) =>
      activeSteps().find(
        (key) => position(key) < position(target) && !init.isComplete(key),
      ),
    next() {
      const next = activeSteps().find(
        (key) => position(key) > position(state.current),
      );
      if (!next) return { kind: "stay" };
      if (!init.isComplete(state.current))
        return { kind: "blocked", step: state.current };
      open(next);
      return { kind: "moved", step: next };
    },
    prev() {
      const previous = activeSteps().findLast(
        (key) => position(key) < position(state.current),
      );
      if (!previous) return { kind: "stay" };
      open(previous);
      return { kind: "moved", step: previous };
    },
    goTo(target) {
      const reachable =
        target !== state.current &&
        activeSteps().includes(target) &&
        position(target) <= position(state.furthest);
      if (!reachable) return { kind: "stay" };
      if (position(target) > position(state.current)) {
        const blocked = activeSteps().find(
          (key) =>
            position(key) >= position(state.current) &&
            position(key) < position(target) &&
            !init.isComplete(key),
        );
        if (blocked) {
          open(blocked);
          return { kind: "blocked", step: blocked };
        }
      }
      open(target);
      return { kind: "moved", step: target };
    },
    open,
    setProcess(process) {
      if (processShape(process) === processShape(state.process)) return;
      // A step the new process cannot open is left for the last one it can
      // open at or before it (else its first), so the frame never shows a
      // step without content whose Next does nothing.
      const active = activeOf(process);
      const order = creationStepOrder(process);
      const at = (key: CreationStepKey) => creationStepNumber(key, order);
      const settle = (key: CreationStepKey) =>
        active.includes(key)
          ? key
          : (active.findLast((candidate) => at(candidate) <= at(key)) ??
            active[0] ??
            key);
      const current = settle(state.current);
      const reached = settle(state.furthest);
      commit({
        process,
        current,
        furthest: at(reached) < at(current) ? current : reached,
      });
    },
  };
}

/** What decides the steps: the process, its order, focus and closed steps. */
const processShape = (process: CreationProcess) =>
  [
    process.key,
    creationStepOrder(process).join(","),
    process.focusGoal ?? "",
    Object.keys(process.inactive).sort().join(","),
  ].join("|");
