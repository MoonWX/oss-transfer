import { expect, test } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";
import { advanceUntilVisible, installPausedClock } from "./clock";
import { mockTaskChildren, revealChildren } from "./task-children-fixture";

const parent = {
  ...tasks[2],
  taskIndex: 1900,
  sourceEndpoint: {
    type: "OSS",
    s3Alias: "production",
    bucket: "training-datasets",
    objectKey: "archive/input.bin",
  },
  localPath: "/mnt/datasets",
  conflictPolicy: "SKIP",
  scanComplete: true,
  status: "RUNNING",
};
const child = {
  ...parent,
  taskIndex: -190001,
  parentTaskId: 1900,
  shardIndex: 0,
  shardId: "1900-local-0",
  executionNodeIp: "10.0.1.12",
  status: "COMPLETED",
  putObjectCount: 42,
  failedObjectCount: 0,
};

test("local summary keeps details available before any child request and loads children only on scroll", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.setViewportSize({ width: 1280, height: 650 });
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "0"),
  );
  const summary = {
    ...parent,
    errorCode: "SOURCE_DISCOVERY_FAILED",
    errorMessage: "源对象读取失败，请检查连接权限。",
  };
  const summaryRequests: string[] = [];
  await page.route("**/api/master/status", (route) => {
    summaryRequests.push(route.request().headers()["x-task-view"] || "");
    return route.fulfill({ json: { totalTasks: 1, tasks: [summary] } });
  });
  let childReads = 0;
  await mockTaskChildren(page, () => {
    childReads++;
    return { parentTaskId: 1900, items: [child] };
  });
  expect(summary).not.toHaveProperty("shards");
  await page.goto("/tasks?taskId=1900");
  const panel = page.locator(".MuiDrawer-paper").filter({
    has: page.getByRole("heading", { name: "迁移任务 #1900", exact: true }),
  });
  await expect(
    panel.getByRole("heading", { name: "迁移任务 #1900", exact: true }),
  ).toBeVisible();
  await expect(
    panel.getByText(summary.errorMessage, { exact: true }),
  ).toBeVisible();
  await expect(panel.getByText("/mnt/datasets", { exact: true })).toBeVisible();
  await expect(panel.getByText("SKIP", { exact: true })).toBeVisible();
  await expect(page.getByTestId("task-children-region")).not.toBeInViewport();
  expect(childReads).toBe(0);
  expect(summaryRequests.length).toBeGreaterThan(0);
  expect(summaryRequests.every((value) => value === "summary")).toBe(true);
  expect(
    requests.some((request) =>
      request.path.includes("/local-transfers/status?taskId=1900"),
    ),
  ).toBe(false);

  await revealChildren(page);
  await expect(
    panel.getByRole("button", { name: "查看子任务 1900-1", exact: true }),
  ).toBeVisible();
  expect(childReads).toBe(1);
  await page.getByRole("button", { name: "关闭详情", exact: true }).click();
  await page
    .getByRole("button", { name: "查看任务 1900", exact: true })
    .click();
  await expect(
    panel.getByRole("heading", { name: "迁移任务 #1900", exact: true }),
  ).toBeVisible();
  expect(childReads).toBe(1);
  await revealChildren(page);
  await expect.poll(() => childReads).toBe(2);
});

test("delayed and failed child refresh keeps the loaded page until a successful newer snapshot", async ({
  page,
}) => {
  await mockApi(page);
  await page.setViewportSize({ width: 1280, height: 650 });
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  await installPausedClock(page);
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: 1, tasks: [parent] } }),
  );
  let release!: () => void;
  const blocked = new Promise<void>((resolve) => {
    release = resolve;
  });
  let mode: "ready" | "blocked" | "recovered" = "ready";
  let reads = 0;
  await mockTaskChildren(page, async () => {
    reads++;
    if (mode === "blocked") {
      await blocked;
      return {
        parentTaskId: 1900,
        items: [],
        error: "执行节点暂时不可达，请重试。",
      };
    }
    return {
      parentTaskId: 1900,
      items: [{ ...child, putObjectCount: mode === "recovered" ? 43 : 42 }],
    };
  });
  await page.goto("/tasks?taskId=1900");
  await advanceUntilVisible(
    page,
    page.getByRole("heading", { name: "迁移任务 #1900", exact: true }),
  );
  await page.clock.runFor(400);
  expect(reads).toBe(0);
  await revealChildren(page);
  await advanceUntilVisible(
    page,
    page.getByRole("button", { name: "查看子任务 1900-1", exact: true }),
  );
  const region = page.getByTestId("task-children-region");
  const card = region.getByRole("button", {
    name: "查看子任务 1900-1",
    exact: true,
  });
  await expect(card).toContainText("已迁移 42");
  mode = "blocked";
  const beforeRefresh = reads;
  await page.clock.runFor(1200);
  await expect.poll(() => reads).toBe(beforeRefresh + 1);
  await expect(card).toContainText("已迁移 42");
  await expect(region.getByText("正在加载子任务…")).toHaveCount(0);
  await expect(region.getByText("尚未生成子任务")).toHaveCount(0);
  release();
  await expect(
    page.getByText("执行节点暂时不可达，请重试。", { exact: true }),
  ).toBeVisible();
  await expect(card).toContainText("已迁移 42");
  await expect(region.getByText("尚未生成子任务")).toHaveCount(0);
  mode = "recovered";
  await revealChildren(page);
  await page.clock.runFor(1200);
  await expect(card).toContainText("已迁移 43");
  await expect(
    page.getByText("执行节点暂时不可达，请重试。", { exact: true }),
  ).toHaveCount(0);
});

test("a failed first child page can be retried inside the visible child region", async ({
  page,
}) => {
  await mockApi(page);
  await page.setViewportSize({ width: 1280, height: 650 });
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "0"),
  );
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: 1, tasks: [parent] } }),
  );
  let reads = 0;
  await mockTaskChildren(page, () => ({
    parentTaskId: 1900,
    items: [child],
    ...(++reads === 1 ? { error: "首次读取子任务失败" } : {}),
  }));
  await page.goto("/tasks?taskId=1900");
  await revealChildren(page);
  const region = page.getByTestId("task-children-region");
  await expect(
    region.getByText("首次读取子任务失败", { exact: true }),
  ).toBeVisible();
  await expect(region.getByText("正在加载子任务…")).toHaveCount(0);
  await region.getByRole("button", { name: "重试", exact: true }).click();
  await expect(
    region.getByRole("button", { name: "查看子任务 1900-1", exact: true }),
  ).toBeVisible();
  expect(reads).toBe(2);
  await expect(
    region.getByText("首次读取子任务失败", { exact: true }),
  ).toHaveCount(0);
});

test("a parent detail error retries the parent while children remain offscreen", async ({
  page,
}) => {
  await mockApi(page);
  await page.setViewportSize({ width: 1280, height: 650 });
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  await installPausedClock(page);
  let fail = false;
  let reads = 0;
  await page.route("**/api/master/status", (route) => {
    reads++;
    return route.fulfill(
      fail
        ? {
            status: 503,
            json: { success: false, message: "主任务状态读取失败" },
          }
        : { json: { totalTasks: 1, tasks: [parent] } },
    );
  });
  let childReads = 0;
  await mockTaskChildren(page, () => {
    childReads++;
    return { parentTaskId: 1900, items: [child] };
  });
  await page.goto("/tasks?taskId=1900");
  const panel = page.getByRole("dialog");
  await advanceUntilVisible(
    page,
    panel.getByRole("heading", { name: "迁移任务 #1900", exact: true }),
  );
  fail = true;
  await page.clock.runFor(1200);
  await expect(
    panel.getByText("主任务状态读取失败", { exact: true }),
  ).toBeVisible();
  await expect(page.getByTestId("task-children-region")).not.toBeInViewport();
  const before = reads;
  fail = false;
  await panel.getByRole("button", { name: "重试", exact: true }).click();
  await expect.poll(() => reads).toBe(before + 1);
  await expect(
    panel.getByText("主任务状态读取失败", { exact: true }),
  ).toHaveCount(0);
  expect(childReads).toBe(0);
});
