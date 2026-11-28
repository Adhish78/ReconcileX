import { act, type ReactElement } from "react";
import { createRoot, type Root } from "react-dom/client";

// Tell React 19 that tests wrap state updates in act().
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

export interface RenderHandle {
  container: HTMLElement;
  unmount: () => Promise<void>;
}

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
