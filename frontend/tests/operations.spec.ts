import { test, expect } from "@playwright/test";
import { mockApi } from "./fixtures";

const failure = {
  taskIndex: 123,
  parentTaskId: 123,
  workerNode: "10.0.1.12",
  sourceBucket: "source",
  destBucket: "dest",
  key: "folder/file.txt",
  objectSize: 1024,
  stage: "WRITE_DESTINATION",
  errorMessage: "Permission denied",
  errorCode: "ACCESS_DENIED",
  timestamp: 1760000000000,
  direction: "OSS_TO_LOCAL",
};

test("converts bandwidth units and can scope rate changes to one node", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.goto("/nodes");
  await expect(page.getByRole("heading", { name: "执行节点" })).toBeVisible();
  await page.getByRole("button", { name: "调整限速", exact: true }).click();
  await page.getByRole("combobox", { name: "限制类型" }).click();
  await page.getByRole("option", { name: "传输带宽" }).click();
  await page.getByRole("spinbutton", { name: "带宽上限（Mbps）" }).fill("25");
  await page.getByRole("button", { name: "应用设置" }).click();
  await expect(page.getByRole("dialog")).toBeHidden();
  expect(
    requests.find((item) => item.path === "/api/master/rate-limit/bandwidth")
      ?.body,
  ).toEqual({ qps: 25_000_000 });
  await page
    .getByRole("button", { name: "调整 master (10.0.1.12) 的 PUT 请求 限速" })
    .click();
  await page.getByRole("spinbutton", { name: "请求上限（次/秒）" }).fill("0");
  await page.getByRole("button", { name: "应用设置" }).click();
  await expect(page.getByRole("dialog")).toBeHidden();
  expect(
    requests.find(
      (item) => item.path === "/api/master/rate-limit/put/10.0.1.12",
    )?.body,
  ).toEqual({ qps: 0 });
});

test("opens local failures from task links, paginates, and reports partial retries", async ({
  page,
}) => {
  await mockApi(page);
  const queries: URL[] = [];
  await page.route("**/api/master/tasks/123", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: { status: "PARTIAL_FAILED", generation: 1 },
      },
    }),
  );
  await page.route(
    "**/api/master/local-transfers/failures?**",
    async (route) => {
      queries.push(new URL(route.request().url()));
      await route.fulfill({
        json: {
          success: true,
          data: {
            items: [failure],
            total: 26,
            partial: true,
            unreachableWorkers: ["10.0.1.13"],
          },
        },
      });
    },
  );
  await page.route("**/api/master/local-transfers/retry-failures", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          accepted: 1,
          ignored: 0,
          failed: 0,
          details: [
            { taskId: 123, generation: 2, status: "RESUMING", accepted: true },
          ],
          partial: true,
          unreachableWorkers: ["10.0.1.13"],
        },
      },
    }),
  );
  await page.goto("/exceptions?taskId=123&type=local");
  await expect(page.getByRole("tab", { name: "本地迁移失败" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  await expect(page.getByLabel("共享存储 ID")).toHaveCount(0);
  await expect(page.getByRole("spinbutton", { name: "任务 ID" })).toHaveValue(
    "123",
  );
  await expect(
    page.getByText("Permission denied", { exact: true }),
  ).toBeVisible();
  expect(queries.some((url) => url.searchParams.get("taskId") === "123")).toBe(
    true,
  );
  await expect(
    page.getByText(
      "部分节点未返回数据，当前结果不完整。不可达节点：agent (10.0.1.13)",
    ),
  ).toBeVisible();
  await page.getByRole("button", { name: "下一页" }).click();
  await expect
    .poll(() => queries.some((url) => url.searchParams.get("offset") === "25"))
    .toBe(true);
  const pagination = page.getByRole("navigation", {
    name: "分页",
    exact: true,
  });
  await expect(pagination).toContainText("26–26 / 26");
  await pagination.getByRole("combobox", { name: "每页" }).click();
  await page.getByRole("option", { name: "10", exact: true }).click();
  await expect(pagination).toContainText("21–26 / 26");
  expect(queries.at(-1)?.searchParams.get("offset")).toBe("20");
  expect(queries.at(-1)?.searchParams.get("limit")).toBe("10");
  await pagination.getByRole("spinbutton", { name: "跳至页码" }).fill("1");
  await pagination.getByRole("spinbutton", { name: "跳至页码" }).press("Enter");
  await expect(pagination).toContainText("1–10 / 26");
  expect(queries.at(-1)?.searchParams.get("offset")).toBe("0");
  expect(queries.at(-1)?.searchParams.get("taskId")).toBe("123");
  await page.reload();
  await expect(pagination.getByRole("combobox", { name: "每页" })).toHaveText(
    "10",
  );
  await page.getByRole("button", { name: "重试任务" }).click();
  await expect(
    page
      .getByRole("main")
      .getByText("重试已受理 1，忽略 0，失败 0；不可达节点：agent (10.0.1.13)"),
  ).toBeVisible();
});

test("exception details distinguish the local parent task from its execution shard", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/local-transfers/failures?**", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          items: [{ ...failure, taskIndex: -184026554, childTaskId: "123-19" }],
          total: 1,
        },
      },
    }),
  );
  await page.route("**/api/master/failed", (route) =>
    route.fulfill({
      json: {
        success: true,
        failedKeys: {
          "10.0.1.12": [{ ...failure, taskIndex: 789, parentTaskId: null }],
          "10.0.1.12Count": 1,
          "10.0.1.13": [],
          "10.0.1.13Count": 0,
        },
      },
    }),
  );
  await page.goto("/exceptions?taskId=123&type=local");
  await page.getByRole("button", { name: "详情", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "异常详情" });
  await expect(
    dialog.getByText("任务 ID", { exact: true }).locator("..").locator("pre"),
  ).toHaveText("123");
  await expect(
    dialog.getByText("子任务 ID", { exact: true }).locator("..").locator("pre"),
  ).toHaveText("123-19");
  await expect(dialog.getByText("父任务 ID", { exact: true })).toHaveCount(0);
  await dialog.getByRole("button", { name: "关闭", exact: true }).click();
  await page.goto("/exceptions?taskId=789&type=oss");
  await page.getByRole("button", { name: "详情", exact: true }).click();
  await expect(
    dialog.getByText("任务 ID", { exact: true }).locator("..").locator("pre"),
  ).toHaveText("789");
  await expect(dialog.getByText("子任务 ID", { exact: true })).toHaveCount(0);
});

test("warns about missing workers and requires confirmation before clearing all records", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.route("**/api/master/failed", (route) =>
    route.fulfill({
      json: {
        success: true,
        queriedWorkers: ["10.0.1.12", "10.0.1.13"],
        failedKeys: { "10.0.1.12": [failure], "10.0.1.12Count": 1 },
      },
    }),
  );
  await page.goto("/exceptions?taskId=123&type=oss");
  await expect(
    page.getByText("Permission denied", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText(
      "部分节点未返回数据，当前结果不完整。不可达节点：agent (10.0.1.13)",
    ),
  ).toBeVisible();
  await page.getByRole("button", { name: "清除全部记录" }).click();
  await expect(
    page.getByText("清除所有执行节点的记录，不受当前筛选条件限制。", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(
    page.getByText(/清除范围也包括本地迁移产生的失败对象/),
  ).toBeVisible();
  expect(
    requests.some((item) => item.path === "/api/master/failed/clear"),
  ).toBe(false);
  await page.getByRole("button", { name: "取消", exact: true }).click();
  expect(
    requests.some((item) => item.path === "/api/master/failed/clear"),
  ).toBe(false);
  const download = page.waitForEvent("download");
  await page.getByRole("button", { name: "导出已加载记录" }).click();
  expect((await download).suggestedFilename()).toBe("failed-records.csv");
  await page.getByRole("button", { name: "清除全部记录" }).click();
  await page.getByRole("button", { name: "确认清除全部" }).click();
  await expect(page.getByRole("dialog")).toBeHidden();
  expect(
    requests.some((item) => item.path === "/api/master/failed/clear"),
  ).toBe(true);
});

test("deletes only selected recycle task IDs", async ({ page }) => {
  const requests = await mockApi(page);
  const task = {
    taskIndex: 123,
    sourceBucket: "source",
    destBucket: "dest",
    status: "PARTIAL_FAILED",
    putObjectCount: 10,
    putObjectSize: 1000,
  };
  await page.route("**/api/master/deleted-tasks", (route) =>
    route.fulfill({
      json: {
        success: true,
        deletedTasks: {
          "10.0.1.12": requests.some(
            (item) => item.path === "/api/master/permanent-delete",
          )
            ? []
            : [task],
          "10.0.1.13": [],
        },
        totalCount: requests.some(
          (item) => item.path === "/api/master/permanent-delete",
        )
          ? 0
          : 1,
      },
    }),
  );
  await page.goto("/tools?tab=recycle");
  await page
    .getByRole("checkbox", { name: "选择任务 123", exact: true })
    .check();
  await page.getByRole("button", { name: "永久删除", exact: true }).click();
  expect(
    requests.some((item) => item.path === "/api/master/permanent-delete"),
  ).toBe(false);
  await page.getByRole("button", { name: "确认永久删除", exact: true }).click();
  await expect(page.getByText("暂无已删除任务", { exact: true })).toBeVisible();
  expect(
    requests.find((item) => item.path === "/api/master/permanent-delete")?.body,
  ).toEqual({ taskIds: [123] });
});

test("operations pages fit narrow screens without document overflow or runtime errors", async ({
  page,
}) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await mockApi(page);
  await page.setViewportSize({ width: 390, height: 844 });
  for (const route of ["nodes", "exceptions?taskId=1046&type=local", "tools"]) {
    await page.goto(`/${route}`);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    if (route === "nodes")
      await expect(page.getByText("请求与带宽").first()).toBeVisible();
    if (route.startsWith("exceptions"))
      await expect(
        page.getByText("Connection reset", { exact: true }),
      ).toBeVisible();
    const dimensions = await page.evaluate(() => ({
      scroll: document.documentElement.scrollWidth,
      viewport: window.innerWidth,
    }));
    expect(dimensions.scroll).toBeLessThanOrEqual(dimensions.viewport);
  }
  expect(errors).toEqual([]);
});
