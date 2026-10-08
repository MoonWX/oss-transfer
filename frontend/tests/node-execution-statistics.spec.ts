import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

const executionTaskStats = {
  scope: "CURRENT_EXECUTION_SUBTASKS",
  totalTasks: 10,
  pendingTasks: 1,
  runningTasks: 1,
  listDoneTasks: 2,
  completedTasks: 3,
  partialFailedTasks: 1,
  failedTasks: 1,
  stoppedTasks: 1,
  unknownTasks: 0,
};

test("nodes distinguish current execution subtasks from legacy records and main task counts", async ({
  page,
}, testInfo) => {
  await mockApi(page);
  await page.route("**/api/master/worker-status", (route) =>
    route.fulfill({
      json: {
        success: true,
        workers: {
          "10.0.1.12": {
            success: true,
            taskStats: { completedTasks: 99 },
            executionTaskStats,
          },
          "10.0.1.13": {
            success: true,
            taskStats: { pendingTasks: 2, runningTasks: 4, completedTasks: 8 },
          },
        },
      },
    }),
  );
  await page.goto("/nodes");
  const current = page.getByRole("region", {
    name: "执行节点 10.0.1.12",
    exact: true,
  });
  await expect(
    current.getByText("子任务执行统计", { exact: true }),
  ).toBeVisible();
  await expect(
    current.getByText("已完成子任务", { exact: true }).locator(".."),
  ).toContainText("3");
  await expect(
    current.getByText("进行中子任务", { exact: true }).locator(".."),
  ).toContainText("3");
  await expect(
    current.getByText("当前保留 10 个子任务", { exact: false }),
  ).toBeVisible();
  await expect(current.getByText("99", { exact: true })).toHaveCount(0);
  await expect(page.getByRole("tooltip")).toHaveCount(0);
  const currentInfo = current.getByRole("button", {
    name: "查看子任务统计口径",
  });
  await currentInfo.focus();
  await expect(page.getByRole("tooltip")).toBeVisible();
  await expect(page.getByRole("tooltip")).toContainText("移入回收站后不再计数");
  await expect(page.getByRole("tooltip")).toContainText(
    "已完成不是主任务累计完成数",
  );
  await currentInfo.evaluate((element) => (element as HTMLElement).blur());
  await expect(page.getByRole("tooltip")).toBeHidden();
  const statistics = current
    .getByText("子任务执行统计", { exact: true })
    .locator("../..");
  await statistics.screenshot({
    path: testInfo.outputPath("node-execution-statistics-desktop.png"),
    scale: "css",
  });
  const legacy = page.getByRole("region", {
    name: "执行节点 10.0.1.13",
    exact: true,
  });
  await expect(
    legacy.getByText("节点记录（旧版统计）", { exact: true }),
  ).toBeVisible();
  await expect(
    legacy.getByText("已完成记录", { exact: true }).locator(".."),
  ).toContainText("8");
  await expect(legacy.getByText("已完成子任务", { exact: true })).toHaveCount(
    0,
  );
  await expect(legacy.getByText("旧版记录，非主任务完成数")).toBeVisible();
  const legacyInfo = legacy.getByRole("button", { name: "查看旧版统计口径" });
  await legacyInfo.hover();
  await expect(page.getByRole("tooltip")).toContainText(
    "不能据此判断主任务完成数量",
  );
  await page.mouse.move(0, 0);
  await expect(page.getByRole("tooltip")).toBeHidden();
  await page.setViewportSize({ width: 390, height: 844 });
  await statistics.scrollIntoViewIfNeeded();
  await statistics.screenshot({
    path: testInfo.outputPath("node-execution-statistics-mobile.png"),
    scale: "css",
  });
  await currentInfo.hover();
  await expect(page.getByRole("tooltip")).toBeVisible();
  await expect(page.getByRole("tooltip")).toContainText(
    "不含主任务及扫描调度记录",
  );
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(390);
});

test("removing retained completed executions updates current counts without implying a lifetime total", async ({
  page,
}) => {
  await mockApi(page);
  let completed = 3;
  await page.route("**/api/master/worker-status", (route) =>
    route.fulfill({
      json: {
        success: true,
        workers: {
          "10.0.1.12": {
            success: true,
            executionTaskStats: {
              ...executionTaskStats,
              completedTasks: completed,
              totalTasks: 7 + completed,
            },
          },
        },
      },
    }),
  );
  await page.goto("/nodes");
  const completedMetric = page
    .getByText("已完成子任务", { exact: true })
    .locator("..");
  await expect(completedMetric).toContainText("3");
  completed = 0;
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(completedMetric).toContainText("0");
  await expect(
    page.getByText("当前保留 7 个子任务", { exact: false }),
  ).toBeVisible();
});
