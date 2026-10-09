import { expect, test, type Locator } from "@playwright/test";
import { mockApi } from "./fixtures";

const worker = {
  success: true,
  rateLimit: {
    list: { limitQps: 100, manualLimitQps: 100, currentQps: 24 },
    put: { limitQps: 1400, manualLimitQps: 1400, currentQps: 216 },
    head: { limitQps: 2800, manualLimitQps: 2800, currentQps: 198 },
    bandwidth: {
      limitBps: 2_000_000_000,
      manualLimitBps: 2_000_000_000,
      currentBps: 648_000_000,
    },
  },
  queueStatus: {
    listQueueSize: 24,
    listQueueCapacity: 10000,
    putQueueSize: 642,
    putQueueCapacity: 1000000,
    progressQueueSize: 12,
    progressQueueCapacity: 10000,
    failedKeyQueueSize: 0,
    failedKeyQueueCapacity: 10000,
    fallbackQueueSize: 0,
    fallbackQueueCapacity: 10000,
  },
};

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

async function expectNoExecutionCounters(region: Locator) {
  await expect(
    region.getByText(
      /子任务执行统计|节点记录（旧版统计）|进行中子任务|待执行子任务|已完成子任务|执行中记录|待执行记录|已完成记录|当前保留|旧版记录，非主任务完成数/,
    ),
  ).toHaveCount(0);
  await expect(
    region.getByRole("button", { name: /查看.*统计口径/ }),
  ).toHaveCount(0);
}

test("nodes show rates and queue load without current or legacy subtask counters", async ({
  page,
}, testInfo) => {
  await mockApi(page);
  await page.route("**/api/master/nodes", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          nodes: [
            { address: "10.0.1.12", master: true },
            { address: "10.0.1.13", agent: true, online: true },
          ],
        },
      },
    }),
  );
  await page.route("**/api/master/worker-status", (route) =>
    route.fulfill({
      json: {
        success: true,
        workers: {
          "10.0.1.12": {
            ...worker,
            taskStats: { completedTasks: 99 },
            executionTaskStats,
          },
          "10.0.1.13": {
            ...worker,
            taskStats: { pendingTasks: 2, runningTasks: 4, completedTasks: 8 },
          },
        },
      },
    }),
  );
  await page.goto("/nodes");
  for (const ip of ["10.0.1.12", "10.0.1.13"]) {
    const region = page.getByRole("region", {
      name: `执行节点 ${ip}`,
      exact: true,
    });
    await expect(region.getByText("在线", { exact: true })).toBeVisible();
    await expectNoExecutionCounters(region);
    for (const type of ["list", "put", "head", "bandwidth"])
      await expect(region.getByTestId(`rate-${ip}-${type}`)).toBeVisible();
    await expect(region.getByText("队列占用", { exact: true })).toBeVisible();
    await expect(
      region.getByText("642 / 1,000,000", { exact: true }),
    ).toBeVisible();
    await expect(
      region.getByRole("progressbar", { name: "对象传输队列占用" }),
    ).toBeVisible();
  }
  const first = page.getByRole("region", {
    name: "执行节点 10.0.1.12",
    exact: true,
  });
  await first
    .getByRole("button", { name: "调整 10.0.1.12 的 传输带宽 限速" })
    .click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByLabel("带宽上限（Mbps）")).toHaveValue("2000");
  await dialog.getByRole("button", { name: "取消", exact: true }).click();
  await page.setViewportSize({ width: 390, height: 844 });
  await first.scrollIntoViewIfNeeded();
  await expectNoExecutionCounters(first);
  await first.screenshot({
    path: testInfo.outputPath("node-load-mobile.png"),
    scale: "css",
  });
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(390);

  await page.goto("/");
  await expect(page.getByRole("heading", { name: "迁移概览" })).toBeVisible();
  await expect(page.getByText("执行中任务", { exact: true })).toBeVisible();
  await expect(page.getByText("已迁移数据", { exact: true })).toBeVisible();
  await expect(
    page.getByText("失败对象", { exact: true }).first(),
  ).toBeVisible();
});

test("node refresh updates rates and queues without bringing back completed counters", async ({
  page,
}) => {
  await mockApi(page);
  let refreshed = false;
  await page.route("**/api/master/worker-status", (route) =>
    route.fulfill({
      json: {
        success: true,
        workers: {
          "10.0.1.12": {
            ...worker,
            executionTaskStats: {
              ...executionTaskStats,
              completedTasks: refreshed ? 0 : 3,
            },
            rateLimit: {
              ...worker.rateLimit,
              bandwidth: {
                ...worker.rateLimit.bandwidth,
                currentBps: refreshed ? 720_000_000 : 648_000_000,
              },
            },
            queueStatus: {
              ...worker.queueStatus,
              putQueueSize: refreshed ? 321 : 642,
            },
          },
        },
      },
    }),
  );
  await page.goto("/nodes");
  const region = page.getByRole("region", {
    name: "执行节点 10.0.1.12",
    exact: true,
  });
  const bandwidth = region.getByTestId("rate-10.0.1.12-bandwidth");
  await expect(bandwidth.getByText("648 Mbps", { exact: true })).toBeVisible();
  await expectNoExecutionCounters(region);
  refreshed = true;
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(bandwidth.getByText("720 Mbps", { exact: true })).toBeVisible();
  await expect(
    region.getByText("321 / 1,000,000", { exact: true }),
  ).toBeVisible();
  await expectNoExecutionCounters(region);
});
