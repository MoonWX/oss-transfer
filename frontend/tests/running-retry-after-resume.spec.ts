import { expect, test } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

// After "重试失败项" is accepted the task runs again. Failures of that new run
// can be retried in place in the same tab; until a child of the run fails,
// the pages say there is nothing to retry rather than "处理中…".
test("failures of a resumed run can be retried in place in the tab that resumed it", async ({
  page,
}) => {
  await mockApi(page);
  let task: Record<string, unknown> = {
    ...tasks[0],
    taskIndex: 1,
    generation: 1,
    status: "PARTIAL_FAILED",
    failedObjectCount: 5,
    failedChildCount: 2,
  };
  let resumed = 0;
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: 1, tasks: [task] } }),
  );
  await page.route("**/api/master/tasks/1", (route) =>
    route.fulfill({ json: { success: true, data: task } }),
  );
  await page.route("**/api/master/resume", (route) => {
    resumed++;
    // The previous run's failures are still counted until they are retried.
    task = { ...task, generation: 2, status: "RUNNING", failedChildCount: 0 };
    return route.fulfill({
      json: {
        success: true,
        accepted: 1,
        ignored: 0,
        failed: 0,
        details: [
          {
            taskId: 1,
            status: "RESUMING",
            generation: 2,
            success: true,
            accepted: true,
            ignored: false,
          },
        ],
      },
    });
  });
  await page.route("**/api/master/failed", (route) =>
    route.fulfill({
      json: {
        success: true,
        failedKeys: {
          "10.0.1.12": [
            {
              parentTaskId: 1,
              taskIndex: 1,
              childTaskId: "1-3",
              key: "new-failure.bin",
              errorMessage: "Upload failed",
              retryStatus: "NOT_RETRIED",
              workerNode: "10.0.1.12",
              direction: "OSS_TO_OSS",
            },
          ],
          "10.0.1.12Count": 1,
        },
        queriedWorkers: ["10.0.1.12"],
        complete: true,
        totalCount: 1,
      },
    }),
  );
  await page.goto("/tasks?taskId=1");
  const detail = page.locator(".MuiDrawer-paper");
  await detail.getByRole("button", { name: "重试失败项", exact: true }).click();
  await page
    .getByRole("dialog", { name: "重试 1 个任务的失败项？" })
    .getByRole("button", { name: "确认重试", exact: true })
    .click();
  await expect.poll(() => resumed).toBe(1);
  // Running again, with only the previous run's failures: nothing to retry.
  await expect(
    detail.getByRole("button", { name: "停止任务", exact: true }),
  ).toBeEnabled();
  await expect(
    detail.getByRole("button", { name: /^(重试失败项|处理中…)$/ }),
  ).toHaveCount(0);
  // A child of the new run ends with failures.
  task = { ...task, failedObjectCount: 6, failedChildCount: 1 };
  await expect(
    detail.getByRole("button", { name: "重试失败项", exact: true }),
  ).toBeEnabled();

  // The exception page of the same tab offers the same in-place retry.
  await page.goto("/exceptions?taskId=1");
  const actions = page.getByRole("region", { name: "任务操作", exact: true });
  await expect(
    actions.getByRole("button", { name: "重试失败项", exact: true }),
  ).toBeEnabled();
  await expect(
    actions.getByRole("button", { name: "处理中…", exact: true }),
  ).toHaveCount(0);
  // Once that child is retrying, the page says nothing is left to retry.
  task = { ...task, failedChildCount: 0 };
  await expect(
    actions.getByRole("button", { name: "暂无可重试的对象", exact: true }),
  ).toBeDisabled({ timeout: 10000 });
  expect(resumed).toBe(1);
});
