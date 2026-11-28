/**
 * Minimal React 19 DOM test harness for Vitest + jsdom.
 *
 * Avoids extra @testing-library dependencies while ensuring renders, unmounts,
 * and asynchronous state updates from useEffect/fetch execute inside React's act().
 */
import { act, type ReactElement } from "react";
import { createRoot, type Root } from "react-dom/client";

// Inform React 19 that this jsdom test environment wraps state updates in act()
// so React suppresses "not configured to support act(...)" warnings.
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

/** Handle returned by renderIntoDocument so tests can query the DOM and clean up. */
export interface RenderHandle {
  container: HTMLElement;
  unmount: () => Promise<void>;
}

/**
 * Creates a detached-style container attached to document.body, mounts `ui` with
 * React 19's createRoot inside act(), and returns a handle for DOM queries and teardown.
 */
export async function renderIntoDocument(ui: ReactElement): Promise<RenderHandle> {
  const container = document.createElement("div");
  document.body.appendChild(container);

  let root: Root | null = null;

  await act(async () => {
    root = createRoot(container);
    root.render(ui);
  });

  return {
    container,
    unmount: async () => {
      await act(async () => {
        root?.unmount();
      });
      container.remove();
    }
  };
}

/**
 * Repeatedly evaluates `check()` until all assertions pass or `timeoutMs` elapses.
 * Each iteration flushes pending microtasks inside act() so async useEffect fetch
 * resolutions commit to the DOM before assertions run.
 */
export async function waitForCondition(check: () => void, timeoutMs = 1500): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  let lastErr: unknown;

  while (Date.now() < deadline) {
    try {
      await act(async () => {
        await Promise.resolve();
      });

      check();
      return;
    } catch (err) {
      lastErr = err;
      await new Promise((r) => setTimeout(r, 15));
    }
  }

  throw lastErr;
}
