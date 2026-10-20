import { defineConfig } from "@playwright/test";

// Fast catalog and regression checks use no browser or development server.
export default defineConfig({
  testDir: "./tests",
  testMatch: [
    "i18n-catalog.spec.ts",
    "api-auth.spec.ts",
    "exception-records.spec.ts",
    "build-age.spec.ts",
  ],
  workers: 1,
  reporter: "list",
});
