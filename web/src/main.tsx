/**
 * Frontend entrypoint executed by index.html.
 * Imports the global dark-mode stylesheet and mounts <App /> inside React StrictMode
 * so development builds surface unsafe side effects or missing effect cleanups early.
 */
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import App from "./App";
import "./styles.css";

// Locate the #root mount node from index.html and hand rendering control to React 19.
createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>
);
