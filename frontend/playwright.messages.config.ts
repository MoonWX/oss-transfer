import { defineConfig } from "@playwright/test";

// Catalog checks use no browser or development server.
export default defineConfig({
  testDir: "./tests",
  testMatch: "i18n-catalog.spec.ts",
  workers: 1,
  reporter: "list",
});
