/**
 * A deploy replaces the hashed route chunks and their CSS. A tab opened before
 * the deploy still asks for the old file names, so a lazy route fails with
 * "Unable to preload CSS for <url>" (KOMBIFY-HOMELAB-2J) or a module fetch
 * error. Vite announces that failure as `vite:preloadError` before the route
 * throws. The cure is loading the new build, so the app reloads once instead of
 * surfacing the failure; a second failure within a minute is a real outage and
 * stays visible.
 */
const RELOAD_KEY = "homelab.staleBuildReloadAt";
const RELOAD_WINDOW_MS = 60_000;

/** Reloads the page once per minute; returns whether it did. */
export function reloadForNewBuild(
  storage: Pick<Storage, "getItem" | "setItem"> | undefined = globalThis.sessionStorage,
  reload: () => void = () => globalThis.location.reload(),
  now = Date.now(),
): boolean {
  try {
    const last = Number(storage?.getItem(RELOAD_KEY) ?? 0);
    if (Number.isFinite(last) && now - last < RELOAD_WINDOW_MS) return false;
    storage?.setItem(RELOAD_KEY, String(now));
  } catch {
    // Storage can be unavailable (private mode); reload once regardless.
  }
  reload();
  return true;
}

/** Installed before the router loads a route, so the stale preload is caught. */
export function installStaleBuildReload(target: Pick<Window, "addEventListener"> = window): void {
  target.addEventListener("vite:preloadError", (event) => {
    if (reloadForNewBuild()) event.preventDefault();
  });
}
