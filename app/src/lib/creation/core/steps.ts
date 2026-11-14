/** The five creation steps, in order. A step's number is its place here. */
export const CREATION_STEP_KEYS = [
  "goals",
  "server",
  "access",
  "users",
  "login",
] as const;

export type CreationStepKey = (typeof CREATION_STEP_KEYS)[number];

/** A process that starts elsewhere names its own order of the same steps. */
export function creationStepNumber(
  key: CreationStepKey,
  steps: readonly CreationStepKey[] = CREATION_STEP_KEYS,
): number {
  return steps.indexOf(key) + 1;
}

export function creationStepKey(
  value: number | string | null | undefined,
  steps: readonly CreationStepKey[] = CREATION_STEP_KEYS,
): CreationStepKey | undefined {
  if (typeof value === "number") return steps[value - 1];
  return CREATION_STEP_KEYS.find((key) => key === value);
}
