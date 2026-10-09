import { test, expect } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";
import { revealChildren } from "./task-children-fixture";

test("login, persistent theme, logout and authenticated requests", async ({
  page,
}) => {
  await mockApi(page, { authenticated: false });
  await page.goto("/");
  await page.getByLabel(/^用户名/).fill("admin");
  await page.getByLabel(/^密码/).fill("example-password");
  await page.getByRole("button", { name: "登录", exact: true }).click();
  await expect(page.getByRole("heading", { name: "迁移概览" })).toBeVisible();
  await page.getByRole("button", { name: "切换主题" }).click();
  await page.getByRole("button", { name: "切换主题" }).click();
  await expect
    .poll(() => page.evaluate(() => localStorage.getItem("oss-transfer-theme")))
    .toBe("dark");
  await page.reload();
  await expect(page.locator("html")).toHaveCSS("color-scheme", "dark");
  await page.getByRole("button", { name: "账号设置" }).click();
  await page.getByRole("menuitem", { name: "退出登录" }).click();
  await expect(page.getByRole("heading", { name: "登录控制台" })).toBeVisible();
  expect(
    await page.evaluate(() => sessionStorage.getItem("oss-transfer-token")),
  ).toBeNull();
});

test("tasks filter, scoped start, partial failure and local detail", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.route("**/api/master/start", (route) =>
    route.fulfill({
      json: {
        success: true,
        message: "启动任务已提交，成功: 1, 失败: 1",
        details: ["节点 10.0.1.12: 转发失败"],
      },
    }),
  );
  await page.goto("/tasks");
  await page.getByRole("checkbox", { name: "选择任务 1047" }).check();
  await page.getByRole("button", { name: "启动", exact: true }).click();
  const requestPromise = page.waitForRequest("**/api/master/start");
  await page.getByRole("button", { name: "确认启动" }).click();
  expect((await requestPromise).postDataJSON()).toEqual({ taskIds: [1047] });
  await expect(page.getByText(/部分请求未完成/)).toBeVisible();
  await page.getByRole("button", { name: "取消选择", exact: true }).click();
  await page.getByRole("checkbox", { name: "选择任务 1048" }).check();
  await expect(
    page.getByRole("button", { name: "继续传输", exact: true }),
  ).toBeDisabled();
  await expect(
    page.getByRole("button", { name: "停止", exact: true }),
  ).toBeEnabled();
  await page.getByRole("button", { name: "查看任务 1046" }).click();
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1046" }),
  ).toBeVisible();
  await expect(
    page.getByText("/mnt/datasets", { exact: true }).last(),
  ).toBeVisible();
  expect(requests.some((r) => r.path.includes("/tasks/1046/children"))).toBe(
    false,
  );
  const childrenRequest = page.waitForRequest(
    "**/api/master/tasks/1046/children?offset=0&limit=25",
  );
  await revealChildren(page);
  await childrenRequest;
  await page.getByRole("button", { name: "关闭详情" }).click();
  await page.getByRole("textbox", { name: "搜索任务" }).fill("analytics");
  await expect(
    page.getByRole("button", { name: "#1047", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "#1048", exact: true }),
  ).toHaveCount(0);
  expect(
    requests.some((r) =>
      r.path.includes("/tasks/1046/children?offset=0&limit=25"),
    ),
  ).toBeTruthy();
  expect(
    requests.some((r) =>
      r.path.includes("/local-transfers/status?taskId=1046"),
    ),
  ).toBe(false);
});

test("stopping a local parent with completed shards is not reported as a partial failure", async ({
  page,
}) => {
  await mockApi(page);
  let stopped = false;
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: {
        totalTasks: tasks.length,
        tasks: tasks.map((task) =>
          task.taskIndex === 1046
            ? { ...task, status: stopped ? "STOPPED" : "RUNNING" }
            : task,
        ),
      },
    }),
  );
  await page.route("**/api/master/local-transfers/stop", (route) => {
    stopped = true;
    return route.fulfill({
      json: {
        success: true,
        data: {
          accepted: 107,
          ignored: 11,
          failed: 0,
          partial: false,
          unreachableWorkers: [],
        },
      },
    });
  });
  await page.goto("/tasks");
  await page.getByRole("checkbox", { name: "选择任务 1046" }).check();
  await page.getByRole("button", { name: "停止", exact: true }).click();
  const requestPromise = page.waitForRequest(
    "**/api/master/local-transfers/stop",
  );
  await page.getByRole("button", { name: "确认停止" }).click();
  expect((await requestPromise).postDataJSON()).toEqual({ taskIds: [1046] });
  await expect(page.getByRole("dialog")).toBeHidden();
  await expect(
    page.getByRole("row").filter({
      has: page.getByRole("button", { name: "#1046", exact: true }),
    }),
  ).toContainText("已停止");
  await expect(
    page.getByText(/停止请求已提交，本地执行项已受理 107，忽略 11，失败 0/),
  ).toBeVisible();
  await expect(page.getByText(/部分请求未完成/)).toHaveCount(0);
  await expect(
    page.getByRole("checkbox", { name: "选择任务 1046" }),
  ).not.toBeChecked();
});

test("all routes render without errors and desktop/mobile fit", async ({
  page,
}) => {
  test.setTimeout(90_000);
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await mockApi(page);
  for (const path of [
    "/",
    "/tasks",
    "/create",
    "/storage",
    "/nodes",
    "/exceptions",
    "/tools",
  ]) {
    await page.goto(path);
    await expect(page.locator("main h1")).toBeVisible();
    await expect(page.getByText("页面加载失败，请刷新重试。")).toHaveCount(0);
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
  }
  await page.setViewportSize({ width: 390, height: 844 });
  await expect
    .poll(() => page.evaluate(() => document.documentElement.scrollWidth))
    .toBe(390);
  for (const path of [
    "/",
    "/tasks",
    "/create",
    "/storage",
    "/nodes",
    "/exceptions",
    "/tools",
  ]) {
    await page.goto(path);
    await expect(page.locator("main h1")).toBeVisible();
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
  }
  await page.getByRole("button", { name: "打开导航" }).click();
  await page.getByRole("link", { name: "迁移任务", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "迁移任务", exact: true }),
  ).toBeVisible();
  expect(errors).toEqual([]);
});

test("keyboard skip link preserves the current route", async ({ page }) => {
  await mockApi(page);
  await page.goto("/create");
  await expect(page.locator("main h1")).toBeVisible();
  const link = page.getByRole("link", { name: "跳转到主要内容" });
  await link.focus();
  await link.press("Enter");
  await expect(page).toHaveURL(/\/create$/);
  await expect(page.locator("main")).toBeFocused();
});

test("401 expires session and worker node offers master entry", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/worker/node-info", (route) =>
    route.fulfill({
      json: { isMaster: false, masterIp: "10.0.1.12", masterPort: 1685 },
    }),
  );
  await page.goto("/");
  await expect(
    page.getByRole("link", { name: "打开主节点控制台" }),
  ).toHaveAttribute("href", "http://10.0.1.12:1685");
  await page.unroute("**/api/worker/node-info");
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      status: 401,
      json: { success: false, message: "会话失效" },
    }),
  );
  await page.reload();
  await expect(page.getByRole("heading", { name: "登录控制台" })).toBeVisible();
});

test("empty state and responsive screenshots", async ({ page }, testInfo) => {
  await mockApi(page);
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.goto("/tasks");
  await expect(
    page.getByRole("button", { name: "#1048", exact: true }),
  ).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("desktop-light.png"),
    fullPage: true,
  });
  await page.getByRole("button", { name: "切换主题" }).click();
  await page.mouse.move(0, 0);
  await expect(page.getByRole("tooltip")).toHaveCount(0);
  await expect(page.locator("html")).toHaveCSS("color-scheme", "dark");
  await page.screenshot({
    path: testInfo.outputPath("desktop-dark.png"),
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect
    .poll(() => page.evaluate(() => document.documentElement.scrollWidth))
    .toBe(390);
  await page.screenshot({
    path: testInfo.outputPath("mobile-dark.png"),
    fullPage: true,
  });
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: 0, tasks: [] } }),
  );
  await page.getByRole("button", { name: "刷新任务" }).click();
  await expect(page.getByText("暂无迁移任务")).toBeVisible();
});
