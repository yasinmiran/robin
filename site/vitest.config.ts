import { fileURLToPath } from "node:url";

import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  test: {
    environment: "happy-dom",
    setupFiles: ["./vitest.setup.ts"],
    include: ["src/**/*.test.{ts,tsx}", "scripts/**/*.test.ts"],
    coverage: {
      provider: "v8",
      // Our own code. The shadcn components are vendored as generated,
      // except the table, which was rewritten for the docs.
      include: ["src/lib/**/*.ts", "src/components/*.tsx", "src/components/ui/table.tsx", "scripts/**/*.ts"],
      exclude: ["**/*.test.{ts,tsx}", "src/lib/utils.ts"],
      reporter: ["text", "text-summary"],
      thresholds: { statements: 85 },
    },
  },
});
