import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";
import { advanceUntilVisible, installPausedClock } from "./clock";

const storageKey = "oss-transfer-refresh-seconds";
const refreshChoice = (page: Page) =>
  page.getByRole("combobox", { name: /^自动刷新/ });

async function chooseRefresh(page: Page, option: string, clock = false) {
  await refreshChoice(page).click();
  await page.getByRole("option", { name: option, exact: true }).click();
  if (clock) await page.clock.runFor(400);
  await expect(page.locator(".MuiPopover-root")).toHaveCount(0);
}

test("refresh frequency persists across overview, tasks, nodes and reload with custom mobile controls", async ({
  page,
}, testInfo) => {
  // This case covers three routes, reloads and desktop/mobile controls.
  test.setTimeout(60_000);
  await mockApi(page);
  await page.goto("/tasks");
  await expect(refreshChoice(page)).toHaveText("10 秒");
  await chooseRefresh(page, "暂停");
  await page.getByRole("link", { name: "概览", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "迁移概览", exact: true }),
  ).toBeVisible();
  await expect(refreshChoice(page)).toHaveText("暂停");
  await page.getByRole("link", { name: "执行节点", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "执行节点", exact: true }),
  ).toBeVisible();
  await expect(refreshChoice(page)).toHaveText("暂停");

  await chooseRefresh(page, "自定义…");
  const dialog = page.getByRole("dialog", { name: "自定义刷新间隔" });
  const input = dialog.getByRole("spinbutton", { name: "间隔（秒）" });
  await input.fill("0");
  await dialog.getByRole("button", { name: "应用", exact: true }).click();
  await expect(dialog.getByText("请输入 1–3600 之间的整数。")).toBeVisible();
  expect(
    await page.evaluate((key) => localStorage.getItem(key), storageKey),
  ).toBe("0");
  await input.fill("17");
  await dialog.getByRole("button", { name: "应用", exact: true }).click();
  await expect(dialog).toBeHidden();
  await expect(refreshChoice(page)).toHaveText("自定义（17 秒）");
  expect(
    await page.evaluate((key) => localStorage.getItem(key), storageKey),
  ).toBe("17");

  await page.getByRole("link", { name: "概览", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "迁移概览", exact: true }),
  ).toBeVisible();
  await expect(refreshChoice(page)).toHaveText("自定义（17 秒）");
  await page.reload();
  await expect(refreshChoice(page)).toHaveText("自定义（17 秒）");
  await chooseRefresh(page, "自定义（17 秒）");
  await expect(input).toHaveValue("17");
  await input.fill("23");
  await dialog.getByRole("button", { name: "取消", exact: true }).click();
  await expect(refreshChoice(page)).toHaveText("自定义（17 秒）");
  await expect(dialog).toBeHidden();

  await page.setViewportSize({ width: 360, height: 800 });
  await chooseRefresh(page, "1 秒");
  await page.goto("/nodes");
  await expect(refreshChoice(page)).toHaveText("1 秒");
  await page.reload();
  await expect(refreshChoice(page)).toHaveText("1 秒");
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(360);
  await page.screenshot({
    path: testInfo.outputPath("refresh-controls-mobile.png"),
    fullPage: true,
  });
});

test("changing cadence retains task data and in-flight work without overlapping polls", async ({
  page,
}) => {
  // In-flight, paused and resumed polling share this total clock/interaction budget.
  test.setTimeout(60_000);
  await installPausedClock(page);
  const requests = await mockApi(page);
  await page.goto("/tasks");
  const task = page.getByRole("button", { name: "#1048", exact: true });
  await advanceUntilVisible(page, task);
  await expect(task).toBeVisible();
  await expect(page.getByRole("button", { name: "刷新任务" })).toBeEnabled();
  const taskElement = await task.elementHandle();
  let calls = 0;
  let hold = true;
  let release!: () => void;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route("**/api/master/status", async (route) => {
    calls++;
    if (hold) await gate;
    await route.fulfill({ json: { totalTasks: tasks.length, tasks } });
  });

  await chooseRefresh(page, "30 秒", true);
  expect(calls).toBe(0);
  expect(await taskElement!.evaluate((element) => element.isConnected)).toBe(
    true,
  );
  await page.clock.runFor(30_000);
  await expect.poll(() => calls).toBe(1);
  await page.clock.fastForward(60_000);
  expect(calls).toBe(1);
  await chooseRefresh(page, "1 秒", true);
  expect(await taskElement!.evaluate((element) => element.isConnected)).toBe(
    true,
  );
  await page.clock.fastForward(20_000);
  expect(calls).toBe(1);
  await expect(task).toBeVisible();

  hold = false;
  release();
  await expect(page.getByRole("button", { name: "刷新任务" })).toBeEnabled();
  await page.clock.runFor(1_000);
  await expect.poll(() => calls).toBe(2);
  await expect(page.getByRole("button", { name: "刷新任务" })).toBeEnabled();
  await page.clock.runFor(1_000);
  await expect.poll(() => calls).toBe(3);

  await chooseRefresh(page, "暂停", true);
  const pausedCalls = calls;
  const pausedHealth = requests.filter((request) =>
    ["/api/master/worker-status", "/api/master/local-nodes"].includes(
      request.path,
    ),
  ).length;
  await page.clock.fastForward(60_000);
  expect(calls).toBe(pausedCalls);
  expect(
    requests.filter((request) =>
      ["/api/master/worker-status", "/api/master/local-nodes"].includes(
        request.path,
      ),
    ),
  ).toHaveLength(pausedHealth);
  await page.getByRole("button", { name: "查看任务 1046" }).click();
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1046" }),
  ).toBeVisible();
  const detailPath = "/api/master/local-transfers/status?taskId=1046";
  await expect
    .poll(
      () => requests.filter((request) => request.path === detailPath).length,
    )
    .toBe(1);
  await page.clock.fastForward(60_000);
  expect(
    requests.filter((request) => request.path === detailPath),
  ).toHaveLength(1);
});

test("node polling uses one preference for all feeds, pauses in background and keeps manual refresh", async ({
  page,
}) => {
  await installPausedClock(page);
  const requests = await mockApi(page);
  await page.goto("/nodes");
  await advanceUntilVisible(
    page,
    page.getByRole("button", { name: "刷新", exact: true }),
  );
  await expect(
    page.getByRole("button", { name: "刷新", exact: true }),
  ).toBeEnabled();
  const paths = [
    "/api/master/worker-status",
    "/api/master/nodes",
    "/api/master/rate-limit/history?limit=50",
  ];
  const counts = () =>
    paths.map(
      (path) => requests.filter((request) => request.path === path).length,
    );
  await chooseRefresh(page, "暂停", true);
  const paused = counts();
  await page.clock.fastForward(60_000);
  expect(counts()).toEqual(paused);
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect.poll(counts).toEqual(paused.map((count) => count + 1));
  await expect(
    page.getByRole("button", { name: "刷新", exact: true }),
  ).toBeEnabled();
  await chooseRefresh(page, "1 秒", true);
  await page.evaluate(() => {
    Object.defineProperty(document, "hidden", {
      configurable: true,
      get: () => true,
    });
  });
  const hidden = counts();
  await page.clock.fastForward(20_000);
  expect(counts()).toEqual(hidden);
  await page.evaluate(() => {
    Object.defineProperty(document, "hidden", {
      configurable: true,
      get: () => false,
    });
  });
  await page.clock.runFor(1_000);
  await expect.poll(counts).toEqual(hidden.map((count) => count + 1));
  await expect(
    page.getByRole("button", { name: "刷新", exact: true }),
  ).toBeEnabled();
  await chooseRefresh(page, "30 秒", true);
  const changed = counts();
  await page.clock.runFor(30_000);
  await expect.poll(counts).toEqual(changed.map((count) => count + 1));
  expect(
    requests.some((request) => request.path === "/api/master/local-nodes"),
  ).toBe(false);
});

test("storage write failure keeps the chosen refresh frequency for page navigation", async ({
  page,
}) => {
  await mockApi(page);
  await page.addInitScript((key) => {
    localStorage.setItem(key, "30");
    const setItem = Storage.prototype.setItem;
    Storage.prototype.setItem = function (name: string, value: string) {
      if (this === localStorage && name === key)
        throw new DOMException("Quota exceeded", "QuotaExceededError");
      setItem.call(this, name, value);
    };
  }, storageKey);
  await page.goto("/tasks");
  await expect(refreshChoice(page)).toHaveText("30 秒");
  await chooseRefresh(page, "5 秒");
  await expect(refreshChoice(page)).toHaveText("5 秒");
  expect(
    await page.evaluate((key) => localStorage.getItem(key), storageKey),
  ).toBe("30");
  await page.getByRole("link", { name: "执行节点", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "执行节点", exact: true }),
  ).toBeVisible();
  await expect(refreshChoice(page)).toHaveText("5 秒");
});
