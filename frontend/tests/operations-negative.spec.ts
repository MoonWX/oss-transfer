import { test, expect } from "@playwright/test";
import { mockApi } from "./fixtures";

test("partial failures expose server-reported nodes even when worker discovery omits them", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/failed", (route) =>
    route.fulfill({
      json: {
        success: true,
        partial: true,
        failedKeys: {
          "10.0.1.12": [],
          "10.0.1.12Count": 0,
          "10.0.1.13": [],
          "10.0.1.13Count": 0,
        },
        failedWorkers: ["10.0.1.99"],
        unreachableWorkers: ["10.0.1.98"],
        workerErrors: { "10.0.1.99": "query rejected" },
      },
    }),
  );
  await page.goto("/exceptions");
  await expect(
    page.getByRole("alert").filter({ hasText: "当前结果不完整" }),
  ).toContainText("10.0.1.99");
  await expect(
    page.getByRole("alert").filter({ hasText: "当前结果不完整" }),
  ).toContainText("10.0.1.98");
  await expect(
    page.getByText("10.0.1.99：query rejected", { exact: true }),
  ).toBeVisible();
});

test("partial clear refreshes available data and leaves a persistent failure rather than success", async ({
  page,
}) => {
  await mockApi(page);
  let reads = 0;
  await page.route("**/api/master/failed", (route) => {
    reads++;
    return route.fulfill({
      json: {
        success: true,
        clearableCount: 1,
        failedKeys: { "10.0.1.12": [], "10.0.1.13": [] },
      },
    });
  });
  await page.route("**/api/master/failed/clear", (route) =>
    route.fulfill({
      json: {
        success: false,
        partial: true,
        message: "部分节点清除失败",
        failedWorkers: ["10.0.1.13"],
      },
    }),
  );
  await page.goto("/exceptions");
  await page.getByRole("button", { name: "清理已处理记录" }).click();
  const readsBeforeClear = reads;
  await page.getByRole("button", { name: "确认清理" }).click();
  await expect(page.getByRole("dialog")).toBeHidden();
  await expect.poll(() => reads).toBeGreaterThan(readsBeforeClear);
  await expect(
    page
      .getByText("部分节点清除失败（未完成节点：agent (10.0.1.13)）", {
        exact: true,
      })
      .first(),
  ).toBeVisible();
  await expect(
    page.getByText("清除请求已提交，正在刷新各节点记录。", { exact: true }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: "关闭", exact: true }).click();
  await expect(
    page.getByText("部分节点清除失败（未完成节点：agent (10.0.1.13)）", {
      exact: true,
    }),
  ).toBeVisible();
});

test("all failed exception queries display failure, not zero exceptions", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/failed", (route) =>
    route.fulfill({
      json: {
        success: false,
        message: "全部节点查询失败",
        failedWorkers: ["10.0.1.12", "10.0.1.13"],
      },
    }),
  );
  await page.goto("/exceptions");
  await expect(
    page.getByRole("alert").filter({ hasText: "全部节点查询失败" }),
  ).toBeVisible();
  await expect(page.getByText("读取失败", { exact: true })).toBeVisible();
  await expect(page.getByText("暂无匹配记录", { exact: true })).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "导出已加载记录" }),
  ).toBeDisabled();
});

test("partial permanent deletion refreshes removed rows and retries only remaining selection", async ({
  page,
}) => {
  await mockApi(page);
  let reads = 0;
  const requests: number[][] = [];
  const task = (taskIndex: number) => ({
    taskIndex,
    sourceBucket: "source",
    destBucket: "destination",
    status: "COMPLETED",
  });
  await page.route("**/api/master/deleted-tasks", (route) => {
    reads++;
    return route.fulfill({
      json: {
        success: true,
        deletedTasks: {
          "10.0.1.12": requests.length === 0 ? [task(123)] : [],
          "10.0.1.13": requests.length < 2 ? [task(124)] : [],
        },
        totalCount: Math.max(0, 2 - requests.length),
      },
    });
  });
  await page.route("**/api/master/permanent-delete", (route) => {
    requests.push(route.request().postDataJSON().taskIds);
    return route.fulfill({
      json:
        requests.length === 1
          ? {
              success: false,
              partial: true,
              accepted: 0,
              failed: 1,
              failedWorkers: ["10.0.1.13"],
              message: "部分节点记录尚未删除，请刷新后重试",
            }
          : { success: true, partial: false, accepted: 1, failed: 0 },
    });
  });
  await page.goto("/tools?tab=recycle");
  await page.getByRole("checkbox", { name: "选择全部已删除任务" }).check();
  await page.getByRole("button", { name: "永久删除", exact: true }).click();
  const readsBeforeDelete = reads;
  await page.getByRole("button", { name: "确认永久删除", exact: true }).click();
  await expect(page.getByRole("dialog")).toBeHidden();
  await expect.poll(() => reads).toBeGreaterThan(readsBeforeDelete);
  await expect(
    page.getByRole("checkbox", { name: "选择任务 123", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("checkbox", { name: "选择任务 124", exact: true }),
  ).toBeChecked();
  const message =
    "部分节点记录尚未删除，请刷新后重试（未完成节点：agent (10.0.1.13)）";
  await expect(page.getByText(message, { exact: true }).first()).toBeVisible();
  await expect(
    page.getByText("永久删除请求已提交，正在刷新回收站。", { exact: true }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: "关闭", exact: true }).click();
  await expect(page.getByText(message, { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "永久删除", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("任务 ID：#124");
  await page.getByRole("button", { name: "确认永久删除", exact: true }).click();
  await expect(page.getByText("暂无已删除任务", { exact: true })).toBeVisible();
  expect(requests.map((ids) => [...ids].sort((a, b) => a - b))).toEqual([
    [123, 124],
    [124],
  ]);
});

test("rate limits reject empty, negative and overflowing bandwidth before sending a request", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.goto("/nodes");
  await page.getByRole("button", { name: "调整限速", exact: true }).click();
  await page.getByRole("combobox", { name: "限制类型" }).click();
  await page.getByRole("option", { name: "传输带宽" }).click();
  const input = page.getByRole("spinbutton", { name: "带宽上限（Mbps）" });
  for (const value of ["", "-1", "1e308"]) {
    await input.fill(value);
    await page.getByRole("button", { name: "应用设置" }).click();
    await expect(
      page.getByText("请输入大于或等于 0 的数值。", { exact: true }),
    ).toBeVisible();
    expect(
      requests.filter(
        (row) => row.method === "POST" && row.path.includes("/rate-limit/"),
      ),
    ).toHaveLength(0);
  }
  await input.fill("1.5");
  await page.getByRole("button", { name: "应用设置" }).click();
  await expect(page.getByRole("dialog")).toBeHidden();
  expect(
    requests.find((row) => row.path === "/api/master/rate-limit/bandwidth")
      ?.body,
  ).toEqual({ qps: 1_500_000 });
});
