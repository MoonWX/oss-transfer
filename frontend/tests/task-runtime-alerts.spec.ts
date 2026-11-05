import { expect, test, type Locator, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";
import { advanceUntilVisible, installPausedClock } from "./clock";
import { runtimeMessageRules } from "../src/lib/runtimeMessages";
import { buildCatalog } from "../src/locales/index.ts";

type Json = Record<string, any>;

const RETIRED = "this node is permanently retired from the parent task";
const LEASE =
  "Waiting for the execution lease of unavailable node scanner (10.0.1.12) to expire";
const SCANNER =
  "Waiting for scanning node scanner (10.0.1.12) to reconnect; existing child tasks remain assigned to their original nodes";
const OLD_LEASE = "Waiting for an unavailable node's execution lease to expire";

/**
 * The task detail polls /api/master/status every second on a paused clock.
 * `row(n)` describes task #1048 in the n-th response; every response carries
 * its number as the storage ID, so a test knows which one is on screen.
 */
async function setup(
  page: Page,
  row: (n: number) => Json,
  refresh = "1",
  routes?: () => Promise<void>,
) {
  await installPausedClock(page);
  await page.addInitScript(
    (seconds) => localStorage.setItem("oss-transfer-refresh-seconds", seconds),
    refresh,
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
  await routes?.();
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

/** Advances the paused clock one second at a time until the target shows. */
async function secondsUntilVisible(page: Page, target: Locator, limit = 20) {
  for (let second = 0; second < limit; second++) {
    if (await target.isVisible()) return second;
    await page.clock.runFor(1000);
  }
  await expect(target).toBeVisible();
  return limit;
}

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
    "等待不可用节点 scanner (10.0.1.12) 的执行租约到期",
    "等待扫描节点 scanner (10.0.1.12) 重新连接；已分配的子任务仍保留在原节点",
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
      errorMessage: `Agent planned-b (10.0.1.13) is unavailable; its assigned work remains on that node: context deadline exceeded\n${OLD_LEASE}\nunexpected EOF`,
    }));
    while (!(await alert(page).isVisible())) {
      await nextRefresh(page, responses);
      expect(responses()).toBeLessThan(20);
    }
    await expect(alert(page).locator("p")).toHaveText([
      "Agent planned-b (10.0.1.13) is unavailable; its assigned work stays on it: The request timed out",
      OLD_LEASE,
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

test("incomplete data from the child page is observed even without a new task snapshot", async ({
  page,
}) => {
  // Real time: scrolling the child region into view needs running timers.
  // Automatic refresh is off, so the task snapshot never changes afterwards.
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "0"),
  );
  await mockApi(page);
  let responses = 0;
  await page.route("**/api/master/status", (route) => {
    responses++;
    return route.fulfill({
      json: {
        success: true,
        totalTasks: tasks.length,
        tasks: tasks.map((task) =>
          task.taskIndex === 1048 ? { ...task, ossTaskGroup: true } : task,
        ),
      },
    });
  });
  let release: () => void = () => {};
  const held = new Promise<void>((resolve) => (release = resolve));
  let childReads = 0;
  await page.route("**/api/master/tasks/1048/children*", async (route) => {
    childReads++;
    await held;
    await route.fulfill({
      json: {
        success: true,
        data: {
          parentTaskId: 1048,
          offset: 0,
          limit: 25,
          items: [],
          totalCount: 0,
          partial: true,
        },
      },
    });
  });
  await page.goto("/tasks?taskId=1048");
  await page.getByTestId("task-children-region").scrollIntoViewIfNeeded();
  await expect.poll(() => childReads).toBeGreaterThan(0);
  const before = responses;
  release();
  // After the four-second stable period; the timeout is only a hang guard.
  await expect(
    page.getByText("任务信息不完整，请刷新或检查执行节点。"),
  ).toBeVisible({ timeout: 20_000 });
  // Only the child page changed; no new task snapshot arrived meanwhile.
  expect(responses).toBe(before);
});

test("an ended task's node-limit mismatch shows at once", async ({ page }) => {
  await installPausedClock(page);
  await mockApi(page);
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: {
        success: true,
        totalTasks: tasks.length,
        tasks: tasks.map((task) =>
          task.taskIndex === 1048 ? { ...task, status: "STOPPED" } : task,
        ),
      },
    }),
  );
  await page.route("**/api/master/tasks/1048/bandwidth-limit", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          limitMbps: 101,
          applied: false,
          pending: false,
          sharedTaskCount: 1,
          canEdit: true,
          nodes: [{ address: "10.0.1.13", applied: false, pending: false }],
        },
      },
    }),
  );
  await page.goto("/tasks?taskId=1048");
  await advanceUntilVisible(page, page.getByText("带宽上限 101 Mbps"));
  // No further clock time: the warning must not wait for a stable period.
  await expect(page.getByText("部分节点未生效")).toBeVisible();
});
