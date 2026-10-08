import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

test("session recovery retries the original route and return to login clears local credentials", async ({
  page,
}) => {
  const requests = await mockApi(page);
  let available = false,
    checks = 0;
  await page.route("**/api/auth/current", async (route) => {
    checks++;
    if (available) await route.fallback();
    else
      await route.fulfill({
        status: 503,
        json: { message: "会话服务暂不可用" },
      });
  });
  await page.goto("/tasks?origin=session-recovery");
  await expect(page.getByRole("alert")).toContainText("会话服务暂不可用");
  await expect(
    page.getByRole("heading", { name: "迁移任务", exact: true }),
  ).toHaveCount(0);
  expect(requests.some((item) => item.path === "/api/master/status")).toBe(
    false,
  );
  expect(
    await page.evaluate(() => sessionStorage.getItem("oss-transfer-token")),
  ).toBe("test-token");
  const checksBeforeRetry = checks;
  available = true;
  await page.getByRole("button", { name: "重试连接", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "迁移任务", exact: true }),
  ).toBeVisible();
  await expect(page).toHaveURL(/\/tasks\?origin=session-recovery$/);
  expect(checks).toBe(checksBeforeRetry + 1);
  available = false;
  await page.evaluate(() =>
    sessionStorage.setItem("oss-transfer-create-draft", "pending-draft"),
  );
  await page.reload();
  await expect(
    page.getByRole("button", { name: "返回登录", exact: true }),
  ).toBeVisible();
  const checksBeforeReturn = checks;
  await page.getByRole("button", { name: "返回登录", exact: true }).click();
  await expect(page.getByRole("heading", { name: "登录控制台" })).toBeVisible();
  expect(checks).toBe(checksBeforeReturn);
  expect(
    await page.evaluate(() => ({
      token: sessionStorage.getItem("oss-transfer-token"),
      draft: sessionStorage.getItem("oss-transfer-create-draft"),
    })),
  ).toEqual({ token: null, draft: null });
  expect(requests.some((item) => item.method !== "GET")).toBe(false);
});

test("node information retry restores the page without exposing business controls during failure", async ({
  page,
}) => {
  const requests = await mockApi(page);
  let available = false,
    nodeCalls = 0;
  await page.route("**/api/worker/node-info", async (route) => {
    nodeCalls++;
    if (available) await route.fallback();
    else
      await route.fulfill({
        status: 503,
        json: { message: "主节点信息读取失败" },
      });
  });
  await page.goto("/create");
  const error = page
    .getByRole("alert")
    .filter({ hasText: "主节点信息读取失败" });
  await expect(error).toBeVisible();
  await expect(
    page.getByRole("button", { name: "创建迁移任务", exact: true }),
  ).toHaveCount(0);
  expect(requests.some((item) => item.path === "/api/master/s3-config")).toBe(
    false,
  );
  const callsBeforeRetry = nodeCalls;
  available = true;
  await error.getByRole("button", { name: "重试", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "创建迁移", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("combobox", { name: "源存储连接" }),
  ).toBeVisible();
  await expect(error).toHaveCount(0);
  expect(nodeCalls).toBe(callsBeforeRetry + 1);
  expect(requests.some((item) => item.method !== "GET")).toBe(false);
});

test("create-page connection and local-node retries target their own reads and retain the draft", async ({
  page,
}) => {
  const requests = await mockApi(page);
  let connectionsReady = false,
    nodesReady = false,
    connectionReads = 0,
    nodeReads = 0;
  await page.route("**/api/master/s3-config", async (route) => {
    connectionReads++;
    if (connectionsReady) await route.fallback();
    else
      await route.fulfill({
        status: 503,
        json: { message: "连接清单暂不可用" },
      });
  });
  await page.route("**/api/master/local-nodes", async (route) => {
    nodeReads++;
    if (nodesReady) await route.fallback();
    else
      await route.fulfill({
        status: 503,
        json: { message: "本地节点清单暂不可用" },
      });
  });
  await page.goto("/create");
  const connectionError = page
    .getByRole("alert")
    .filter({ hasText: "无法加载存储连接" });
  await expect(connectionError).toContainText("连接清单暂不可用");
  await expect(page.getByText("创建任务前需添加存储连接。")).toHaveCount(0);
  await page.getByRole("button", { name: /对象存储 → 本地/ }).click();
  await page.getByLabel("目标目录绝对路径").fill("/mnt/datasets/retry-draft");
  const nodeError = page
    .getByRole("alert")
    .filter({ hasText: "无法获取节点信息" });
  await expect(nodeError).toContainText("本地节点清单暂不可用");
  const nodeReadsBefore = nodeReads;
  const connectionsBeforeRetry = connectionReads;
  connectionsReady = true;
  await connectionError
    .getByRole("button", { name: "重试", exact: true })
    .click();
  await expect(connectionError).toHaveCount(0);
  expect(connectionReads).toBe(connectionsBeforeRetry + 1);
  expect(nodeReads).toBe(nodeReadsBefore);
  await page.getByRole("combobox", { name: "源存储连接" }).click();
  await page.getByRole("option", { name: "production", exact: true }).click();
  const connectionReadsBefore = connectionReads;
  nodesReady = true;
  await nodeError.getByRole("button", { name: "重试", exact: true }).click();
  await expect(nodeError).toHaveCount(0);
  expect(nodeReads).toBe(nodeReadsBefore + 1);
  expect(connectionReads).toBe(connectionReadsBefore);
  await expect(page.getByLabel("目标目录绝对路径")).toHaveValue(
    "/mnt/datasets/retry-draft",
  );
  await expect(
    page.getByRole("combobox", { name: "源存储连接" }),
  ).toContainText("production");
  await page.getByRole("combobox", { name: "执行节点 IP" }).click();
  await expect(
    page.getByRole("option", { name: /10\.0\.1\.12/ }),
  ).toBeVisible();
  expect(requests.some((item) => item.method !== "GET")).toBe(false);
});

test("fallback query and clear use dedicated endpoints with cancel, partial failure and successful retry", async ({
  page,
}) => {
  const requests = await mockApi(page);
  let readable = false,
    clearCount = 0;
  const reads: unknown[] = [];
  const clears: { method: string; body: string | null }[] = [];
  const first = {
    taskIndex: 501,
    key: "fallback/first.bin",
    errorMessage: "首节点备用传输记录",
    tmpFilePath: "/tmp/first.bin",
  };
  const second = {
    taskIndex: 502,
    key: "fallback/second.bin",
    errorMessage: "第二节点备用传输记录",
    tmpFilePath: "/tmp/second.bin",
  };
  await page.route("**/api/master/fallback", async (route) => {
    expect(route.request().method()).toBe("POST");
    reads.push(route.request().postDataJSON());
    if (!readable) {
      await route.fulfill({
        status: 503,
        json: { message: "降级记录读取失败" },
      });
      return;
    }
    await route.fulfill({
      json: {
        success: true,
        fallbackObjects: {
          "10.0.1.12": clearCount === 0 ? [first] : [],
          "10.0.1.12Count": clearCount === 0 ? 1 : 0,
          "10.0.1.13": clearCount < 2 ? [second] : [],
          "10.0.1.13Count": clearCount < 2 ? 1 : 0,
        },
      },
    });
  });
  await page.route("**/api/master/fallback/clear", async (route) => {
    clears.push({
      method: route.request().method(),
      body: route.request().postData(),
    });
    clearCount++;
    await route.fulfill({
      json:
        clearCount === 1
          ? {
              success: false,
              partial: true,
              failedWorkers: ["10.0.1.13"],
              message: "降级清理部分失败",
            }
          : { success: true, partial: false },
    });
  });
  await page.goto("/exceptions");
  await page.getByRole("tab", { name: "降级迁移记录", exact: true }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "降级记录读取失败" }),
  ).toBeVisible();
  await expect(page.getByRole("heading", { name: "暂无匹配记录" })).toHaveCount(
    0,
  );
  await expect(
    page.getByRole("button", { name: "导出已加载记录", exact: true }),
  ).toBeDisabled();
  readable = true;
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(page.getByText(first.key, { exact: true })).toBeVisible();
  await expect(page.getByText(second.key, { exact: true })).toBeVisible();
  expect(reads.at(-1)).toEqual({ limit: 1000 });
  await page.getByRole("button", { name: "清除全部记录", exact: true }).click();
  const confirmation = page.getByRole("dialog", { name: "清除全部降级记录？" });
  await expect(confirmation).toContainText(
    "清除所有执行节点的记录，不受当前筛选条件限制。",
  );
  await confirmation.getByRole("button", { name: "取消", exact: true }).click();
  expect(clears).toHaveLength(0);
  await page.getByRole("button", { name: "清除全部记录", exact: true }).click();
  await confirmation
    .getByRole("button", { name: "确认清除全部", exact: true })
    .click();
  await expect(confirmation).toBeHidden();
  await expect(page.getByText(first.key, { exact: true })).toHaveCount(0);
  await expect(page.getByText(second.key, { exact: true })).toBeVisible();
  await expect(
    page
      .getByText("降级清理部分失败（未完成节点：10.0.1.13）", { exact: true })
      .first(),
  ).toBeVisible();
  await expect(
    page.getByText("清除请求已提交，正在刷新各节点记录。", { exact: true }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: "清除全部记录", exact: true }).click();
  await confirmation
    .getByRole("button", { name: "确认清除全部", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "暂无匹配记录" }),
  ).toBeVisible();
  await expect(
    page.getByText("降级清理部分失败（未完成节点：10.0.1.13）", {
      exact: true,
    }),
  ).toHaveCount(0);
  expect(clears).toEqual([
    { method: "POST", body: null },
    { method: "POST", body: null },
  ]);
  expect(
    requests.some((item) => item.path === "/api/master/failed/clear"),
  ).toBe(false);
});

test("runtime error boundary reload recovers the current route after correcting the service response", async ({
  page,
}) => {
  const requests = await mockApi(page);
  let valid = false,
    statusReads = 0;
  await page.route("**/api/master/status", async (route) => {
    statusReads++;
    if (valid) await route.fallback();
    else
      await route.fulfill({
        json: { totalTasks: 0, tasks: { malformed: true } },
      });
  });
  await page.goto("/tasks?origin=runtime-recovery");
  await expect(
    page.getByText("页面加载失败，请刷新重试。", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "迁移任务", exact: true }),
  ).toHaveCount(0);
  const readsBefore = statusReads;
  valid = true;
  await page.getByRole("button", { name: "重新加载", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "迁移任务", exact: true }),
  ).toBeVisible();
  await expect(page).toHaveURL(/\/tasks\?origin=runtime-recovery$/);
  await expect(
    page.getByText("页面加载失败，请刷新重试。", { exact: true }),
  ).toHaveCount(0);
  expect(statusReads).toBeGreaterThan(readsBefore);
  expect(requests.some((item) => item.method !== "GET")).toBe(false);
});
