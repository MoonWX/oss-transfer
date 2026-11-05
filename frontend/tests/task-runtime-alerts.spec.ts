import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";
import { advanceUntilVisible, installPausedClock } from "./clock";
import { runtimeMessageRules } from "../src/lib/runtimeMessages";
import { buildCatalog } from "../src/locales/index.ts";

type Json = Record<string, any>;

const RETIRED = "this node is permanently retired from the parent task";
const LEASE = "Waiting for an unavailable node's execution lease to expire";
const SCANNER =
  "Waiting for the scanning node to reconnect; existing child tasks remain assigned to their original nodes";

/**
 * The task detail polls /api/master/status every second on a paused clock.
 * `row(n)` describes task #1048 in the n-th response; every response carries
 * its number as the storage ID, so a test knows which one is on screen.
 */
async function setup(page: Page, row: (n: number) => Json) {
  await installPausedClock(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  await mockApi(page);
  let responses = 0;
  await page.route("**/api/master/status", (route) => {
    const n = ++responses;
    return route.fulfill({
      json: {
        success: true,
        totalTasks: tasks.length,
        tasks: tasks.map((task) =>
          task.taskIndex === 1048
            ? { ...task, storageId: `snapshot-${n}`, ...row(n) }
            : task,
        ),
      },
    });
  });
  await page.goto("/tasks?taskId=1048");
  return { responses: () => responses };
}

/** Advances one refresh and waits until that response is on screen. */
async function nextRefresh(page: Page, responses: () => number) {
  const target = responses() + 1;
  await expect
    .poll(async () => {
      if (responses() < target) await page.clock.runFor(250);
      return responses();
    })
    .toBeGreaterThanOrEqual(target);
  await expect(
    page.getByText(`snapshot-${responses()}`, { exact: true }),
  ).toBeVisible();
}

const alert = (page: Page) => page.getByTestId("task-runtime-alert");

test("a runtime error from a single refresh never flashes", async ({
  page,
}) => {
  const { responses } = await setup(page, (n) =>
    n === 4 ? { errorMessage: RETIRED } : { errorMessage: "" },
  );
  for (let i = 0; i < 10; i++) {
    await nextRefresh(page, responses);
    await expect(alert(page)).toHaveCount(0);
  }
  expect(responses()).toBeGreaterThan(5);
});

test("a persistent runtime error appears translated, one line per problem, and survives a single clean refresh", async ({
  page,
}) => {
  let clean = new Set<number>();
  let cleared = Infinity;
  const { responses } = await setup(page, (n) => ({
    errorMessage:
      n >= 3 && n < cleared && !clean.has(n) ? `${LEASE}\n${SCANNER}` : "",
  }));
  while (!(await alert(page).isVisible())) {
    await nextRefresh(page, responses);
    expect(responses()).toBeLessThan(20);
  }
  const lines = alert(page).locator("p");
  await expect(lines).toHaveText([
    "等待不可用节点的执行租约到期",
    "等待扫描节点重新连接；已分配的子任务仍保留在原节点",
  ]);
  await expect(alert(page)).not.toContainText("Waiting");
  // One refresh without the problem does not hide it.
  clean = new Set([responses() + 1]);
  for (let i = 0; i < 3; i++) {
    await nextRefresh(page, responses);
    await expect(alert(page)).toBeVisible();
  }
  // Once it stays away, the alert goes.
  cleared = responses() + 1;
  while (await alert(page).isVisible()) {
    await nextRefresh(page, responses);
    expect(responses()).toBeLessThan(cleared + 15);
  }
  expect(responses()).toBeGreaterThan(cleared);
});

test("an ended task's error is final and shows at once", async ({ page }) => {
  const { responses } = await setup(page, () => ({
    status: "FAILED",
    errorMessage: "AccessDenied: listing source bucket",
  }));
  await advanceUntilVisible(page, page.getByText(/^snapshot-\d+$/));
  await expect(alert(page)).toHaveText(
    "错误详情：AccessDenied: listing source bucket",
  );
  expect(responses()).toBeLessThan(3);
});

test.describe("English", () => {
  test.use({ locale: "en-US" });

  test("known runtime messages use the catalog and unknown ones keep a translated label", async ({
    page,
  }) => {
    const { responses } = await setup(page, () => ({
      errorMessage: `Agent planned-b (10.0.1.13) is unavailable; its assigned work remains on that node: context deadline exceeded\nunexpected EOF`,
    }));
    while (!(await alert(page).isVisible())) {
      await nextRefresh(page, responses);
      expect(responses()).toBeLessThan(20);
    }
    await expect(alert(page).locator("p")).toHaveText([
      "Agent planned-b (10.0.1.13) is unavailable; its assigned work stays on it: The request timed out",
      "Details: unexpected EOF",
    ]);
  });
});

test("a node-limit mismatch from a single refresh never flashes", async ({
  page,
}) => {
  await installPausedClock(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  await mockApi(page);
  let reads = 0;
  await page.route("**/api/master/tasks/1048/bandwidth-limit", (route) => {
    const n = ++reads;
    return route.fulfill({
      json: {
        success: true,
        data: {
          limitMbps: 100 + n,
          applied: n !== 3,
          pending: false,
          sharedTaskCount: 1,
          canEdit: true,
          nodes: [
            {
              address: "10.0.1.13",
              applied: n !== 3,
              pending: false,
              message: n === 3 ? "未生效" : undefined,
            },
          ],
        },
      },
    });
  });
  await page.goto("/tasks?taskId=1048");
  for (let i = 0; i < 8; i++) {
    const target = reads + 1;
    await expect
      .poll(async () => {
        if (reads < target) await page.clock.runFor(250);
        return reads;
      })
      .toBeGreaterThanOrEqual(target);
    await expect(page.getByText(`带宽上限 ${100 + reads} Mbps`)).toBeVisible();
    await expect(page.getByText("部分节点未生效")).toHaveCount(0);
  }
});

test("every runtime message rule has a complete catalog entry", () => {
  for (const language of ["en", "ja"] as const) {
    const catalog = buildCatalog(language);
    for (const rule of runtimeMessageRules) {
      expect(
        Object.hasOwn(catalog, rule.message),
        `${language}: ${rule.message}`,
      ).toBe(true);
      for (const name of rule.values || [])
        expect(catalog[rule.message]).toContain(`{${name}}`);
    }
  }
});
