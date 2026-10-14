import { expect, test } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

const task = (id: number) => ({
  ...tasks[0],
  taskIndex: id,
  childCount: 0,
  scanComplete: true,
});

test("a task outside the loaded list opens by ID and can be stopped", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: {
        success: true,
        tasks: [task(10002)],
        truncated: true,
        partial: true,
        nextBeforeId: 10002,
      },
    }),
  );
  let stopped = false;
  let detailReads = 0;
  await page.route("**/api/master/tasks/7", (route) => {
    detailReads++;
    return route.fulfill({
      json: {
        success: true,
        data: { ...task(7), status: stopped ? "STOPPED" : "RUNNING" },
      },
    });
  });
  await page.route("**/api/master/stop", (route) => {
    expect(route.request().postDataJSON()).toEqual({ taskIds: [7] });
    stopped = true;
    return route.fulfill({ json: { success: true, accepted: 1, failed: 0 } });
  });
  await page.goto("/tasks?taskId=7");
  await expect(page.getByText("迁移任务 #7", { exact: true })).toBeVisible();
  const drawer = page.locator(".MuiDrawer-paper");
  await drawer.getByRole("button", { name: "停止任务", exact: true }).click();
  await page.getByRole("button", { name: "确认停止", exact: true }).click();
  await expect.poll(() => stopped).toBe(true);
  await expect.poll(() => detailReads).toBeGreaterThan(1);
  await expect(
    drawer.getByText("已停止", { exact: true }).first(),
  ).toBeVisible();
});

test("earlier tasks survive refresh and use the returned exclusive cursor", async ({
  page,
}) => {
  await mockApi(page);
  let latest = 10002;
  await page.route("**/api/master/status**", (route) => {
    const cursor = new URL(route.request().url()).searchParams.get("beforeId");
    return route.fulfill({
      json: cursor
        ? { success: true, tasks: [task(7)], truncated: false, partial: false }
        : {
            success: true,
            tasks: [task(latest)],
            truncated: true,
            partial: true,
            nextBeforeId: 10002,
          },
    });
  });
  await page.route("**/api/master/tasks/7", (route) =>
    route.fulfill({ json: { success: true, data: task(7) } }),
  );
  await page.goto("/tasks");
  const earlier = page.waitForRequest((request) =>
    request.url().includes("/api/master/status?beforeId=10002&limit=1000"),
  );
  await page.getByRole("button", { name: "加载更早任务", exact: true }).click();
  await earlier;
  await expect(
    page.getByRole("button", { name: "#7", exact: true }),
  ).toBeVisible();
  latest = 10003;
  await page.getByRole("button", { name: "刷新任务", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "#10003", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "#10002", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "#7", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "#7", exact: true }).click();
  await expect(page.getByText("迁移任务 #7", { exact: true })).toBeVisible();
});

test("recycle history loads earlier records without claiming a node is missing", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/deleted-tasks**", (route) => {
    const older = new URL(route.request().url()).searchParams.has("beforeId");
    return route.fulfill({
      json: {
        success: true,
        deletedTasks: {
          "10.0.1.12": [{ ...task(older ? 7 : 10002), status: "STOPPED" }],
        },
        totalCount: 1,
        queriedWorkers: ["10.0.1.12"],
        complete: older,
        partial: !older,
        truncated: !older,
        nextBeforeId: older ? undefined : 10002,
      },
    });
  });
  await page.goto("/tools?tab=recycle");
  await page.getByRole("button", { name: "加载更早记录", exact: true }).click();
  await expect(page.getByText("#7", { exact: true })).toBeVisible();
  await expect(page.getByText("#10002", { exact: true })).toBeVisible();
  await expect(
    page.getByText("以下节点未返回回收站数据", { exact: false }),
  ).toHaveCount(0);
  await expect(
    page.getByText("部分回收站记录未返回", { exact: false }),
  ).toHaveCount(0);
});

test("bulk stop selects at most 128 parents and allows a later explicit selection", async ({
  page,
}) => {
  await mockApi(page);
  const records = Array.from({ length: 130 }, (_, index) => ({
    ...task(130 - index),
    sourceBucket: index === 129 ? "remaining-one" : "batch-source",
  }));
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { success: true, tasks: records } }),
  );
  const requests: number[][] = [];
  await page.route("**/api/master/stop", (route) => {
    const ids = route.request().postDataJSON().taskIds as number[];
    requests.push(ids);
    return route.fulfill({
      json: { success: true, accepted: ids.length, failed: 0 },
    });
  });
  await page.goto("/tasks");
  await page
    .getByRole("checkbox", { name: "选择任务 130", exact: true })
    .check();
  await page
    .getByRole("button", { name: "选择前 128 个结果", exact: true })
    .click();
  await expect(
    page.getByText("已选 128 项（最多 128 项）", { exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "停止", exact: true }).click();
  await page.getByRole("button", { name: "确认停止", exact: true }).click();
  await expect.poll(() => requests.length).toBe(1);
  expect(requests[0]).toEqual(
    Array.from({ length: 128 }, (_, index) => 130 - index),
  );
  await page.getByPlaceholder("搜索任务、存储桶或路径").fill("remaining-one");
  await page.getByRole("checkbox", { name: "选择任务 1", exact: true }).check();
  await page.getByRole("button", { name: "停止", exact: true }).click();
  await page.getByRole("button", { name: "确认停止", exact: true }).click();
  await expect.poll(() => requests.length).toBe(2);
  expect(requests[1]).toEqual([1]);
});

test("recycle bulk deletion is limited to 128 parents", async ({ page }) => {
  await mockApi(page);
  const records = Array.from({ length: 130 }, (_, index) => ({
    ...task(130 - index),
    status: "STOPPED",
  }));
  await page.route("**/api/master/deleted-tasks", (route) =>
    route.fulfill({
      json: {
        success: true,
        deletedTasks: { "10.0.1.12": records },
        totalCount: records.length,
        queriedWorkers: ["10.0.1.12"],
        complete: true,
      },
    }),
  );
  let submitted: number[] | undefined;
  await page.route("**/api/master/permanent-delete", (route) => {
    submitted = route.request().postDataJSON().taskIds;
    return route.fulfill({
      json: { success: true, accepted: submitted?.length, failed: 0 },
    });
  });
  await page.goto("/tools?tab=recycle");
  await page
    .getByRole("checkbox", { name: "选择前 128 个已删除任务", exact: true })
    .check();
  await expect(
    page.getByText("已选择 128 个任务 ID（最多 128 个）", { exact: false }),
  ).toBeVisible();
  await page.getByRole("button", { name: "永久删除", exact: true }).click();
  await page.getByRole("button", { name: "确认永久删除", exact: true }).click();
  await expect.poll(() => submitted?.length).toBe(128);
  expect(submitted).toEqual(
    Array.from({ length: 128 }, (_, index) => 130 - index),
  );
});
