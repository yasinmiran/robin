import "@testing-library/jest-dom/vitest";

import { cleanup } from "@testing-library/react";
import { afterEach, vi } from "vitest";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.useRealTimers();
  document.documentElement.className = "";
  try {
    localStorage.clear();
  } catch {
    // Some tests replace storage with one that throws.
  }
});
