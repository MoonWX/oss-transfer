import { defineConfig } from "@playwright/test";
import config from "./playwright.config";

// Check duplicate requests, action targets and HTTP 401 handling with mocked APIs.
// npm test keeps the full UI regression suite.
const titles = [
  "401 expires session and worker node offers master entry",
  "synchronous duplicate form submit events send only one creation request",
  ...["OSS_TO_OSS", "OSS_TO_LOCAL"].map(
    (direction) =>
      `${direction} can stop and resume from its own detail without losing counters`,
  ),
];

export default defineConfig(config, {
  testMatch: [
    "**/workspace.spec.ts",
    "**/oss-task-groups.spec.ts",
    "**/task-lifecycle.spec.ts",
  ],
  grep: new RegExp(
    ` (${titles.map((title) => title.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")).join("|")})$`,
  ),
});
