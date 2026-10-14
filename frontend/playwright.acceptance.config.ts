import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./acceptance",
  testMatch: "transfer.spec.ts",
  workers: 1,
  retries: 0,
  timeout: 120_000,
  expect: { timeout: 15_000 },
  outputDir: process.env.ACCEPTANCE_EVIDENCE || "./test-results/acceptance",
  use: {
    locale: "zh-CN",
    baseURL: process.env.ACCEPTANCE_BASE_URL,
    browserName: "chromium",
    viewport: { width: 1440, height: 1000 },
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
    screenshot: "only-on-failure",
    trace: "retain-on-failure",
  },
  reporter: [["list"]],
});
