/**
 * Pre-ES2022 engines lack `Array.prototype.at` (Chrome 92) and
 * `String.prototype.at` (Chrome 92). The vendored agent-model catalog in
 * `@kombiverselabs/execution-contracts` validates itself while it is imported
 * and calls `.at(-1)`, so without this the client entry throws
 * `TypeError: r.at is not a function` before any route renders
 * (KOMBIFY-HOMELAB-4P, /login, Chrome 79).
 *
 * `hooks.client.ts` imports this before the router loads a route node, so the
 * polyfill is defined ahead of every module that might use `.at`.
 */

const toIntegerOrInfinity = (value: unknown): number => {
  const number = Number(value);
  return Number.isFinite(number) ? Math.trunc(number) : 0;
};

function defineAt(target: object, at: (index: number) => unknown): void {
  const existing = (target as { at?: unknown }).at;
  if (typeof existing === "function") return;
  Object.defineProperty(target, "at", {
    value: at,
    writable: true,
    configurable: true,
    enumerable: false,
  });
}

export function installLegacyBrowserSupport(): void {
  defineAt(Array.prototype, function (this: unknown[], index: number) {
    const length = this.length;
    const relative = toIntegerOrInfinity(index);
    const position = relative >= 0 ? relative : length + relative;
    return position < 0 || position >= length ? undefined : this[position];
  });

  defineAt(String.prototype, function (this: string, index: number) {
    const value = String(this);
    const length = value.length;
    const relative = toIntegerOrInfinity(index);
    const position = relative >= 0 ? relative : length + relative;
    return position < 0 || position >= length ? undefined : value[position];
  });
}
