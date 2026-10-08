import { expect, test } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

test("desktop sidebar resizes with pointer and keyboard then remembers its icon rail without replacing the layout", async ({
  page,
}) => {
  await mockApi(page);
  await page.goto("/tasks");
  const nav = page.getByRole("navigation", { name: "主导航" });
  const shell = await page
    .getByRole("button", { name: "账号设置" })
    .elementHandle();
  const tasksLink = await nav
    .getByRole("link", { name: "迁移任务", exact: true })
    .elementHandle();
  const handle = page.getByRole("separator", { name: "调整侧栏宽度" });
  await expect(handle).toHaveAttribute("aria-valuenow", "248");
  const box = (await handle.boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, 140);
  await page.mouse.down();
  await page.mouse.move(box.x + box.width / 2 + 80, 140);
  await page.mouse.up();
  await expect(handle).toHaveAttribute("aria-valuenow", "328");
  await expect.poll(async () => (await nav.boundingBox())!.width).toBe(328);
  await handle.focus();
  await handle.press("End");
  await expect(handle).toHaveAttribute("aria-valuenow", "360");
  await handle.press("ArrowRight");
  await expect(handle).toHaveAttribute("aria-valuenow", "360");
  await handle.press("Home");
  await handle.press("ArrowLeft");
  await expect(handle).toHaveAttribute("aria-valuenow", "216");
  await handle.press("ArrowRight");
  await expect(handle).toHaveAttribute("aria-valuenow", "224");
  await page.getByRole("button", { name: "折叠侧栏", exact: true }).click();
  await expect.poll(async () => (await nav.boundingBox())!.width).toBe(80);
  await expect(handle).toHaveCount(0);
  const storage = nav.getByRole("link", { name: "存储连接", exact: true });
  await expect(storage).toHaveAttribute("title", "存储连接");
  await storage.click();
  await expect(
    page.getByRole("heading", { name: "存储连接", exact: true }),
  ).toBeVisible();
  expect(await shell!.evaluate((element) => element.isConnected)).toBe(true);
  expect(await tasksLink!.evaluate((element) => element.isConnected)).toBe(
    true,
  );
  await page.reload();
  await expect(
    page.getByRole("button", { name: "展开侧栏", exact: true }),
  ).toBeVisible();
  await expect.poll(async () => (await nav.boundingBox())!.width).toBe(80);
  await page.getByRole("button", { name: "展开侧栏", exact: true }).click();
  await expect.poll(async () => (await nav.boundingBox())!.width).toBe(224);
  await handle.dblclick();
  await expect(handle).toHaveAttribute("aria-valuenow", "248");
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(1280);
});

test("mobile navigation remains a full drawer despite the saved desktop icon rail", async ({
  page,
}) => {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem(
      "oss-transfer-sidebar",
      JSON.stringify({ collapsed: true, width: 360 }),
    ),
  );
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/tasks");
  await expect(
    page.getByRole("button", { name: "展开侧栏", exact: true }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: "打开导航", exact: true }).click();
  const link = page.getByRole("link", { name: "存储连接", exact: true });
  await expect(link).toContainText("存储连接");
  const paper = page.locator(".MuiDrawer-paper").filter({ has: link });
  expect((await paper.boundingBox())!.width).toBeCloseTo(264, 2);
  await link.click();
  await expect(
    page.getByRole("heading", { name: "存储连接", exact: true }),
  ).toBeVisible();
  await expect(paper).toHaveCount(0);
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(390);
});

test("sidebar keeps session controls usable with malformed or unwritable local preferences", async ({
  page,
}) => {
  await mockApi(page);
  await page.addInitScript(() => {
    localStorage.setItem("oss-transfer-sidebar", "not-json");
    const original = Storage.prototype.setItem;
    Storage.prototype.setItem = function (key, value) {
      if (key === "oss-transfer-sidebar")
        throw new DOMException("storage disabled", "QuotaExceededError");
      return original.call(this, key, value);
    };
  });
  await page.goto("/tasks");
  await expect(
    page.getByRole("separator", { name: "调整侧栏宽度" }),
  ).toHaveAttribute("aria-valuenow", "248");
  await page.getByRole("button", { name: "折叠侧栏", exact: true }).click();
  await page
    .getByRole("navigation", { name: "主导航" })
    .getByRole("link", { name: "执行节点", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "执行节点", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "展开侧栏", exact: true }),
  ).toBeVisible();
});

test("stopped and ended tasks allow report export while active tasks issue no ticket", async ({
  page,
}) => {
  const requests = await mockApi(page);
  const states = [
    "PENDING",
    "RUNNING",
    "LIST_DONE",
    "STOPPED",
    "COMPLETED",
    "FAILED",
    "PARTIAL_FAILED",
  ];
  const matrix = states.map((status, index) => ({
    ...tasks[0],
    taskIndex: 9000 + index,
    status,
    direction: index % 2 ? "OSS_TO_LOCAL" : "OSS_TO_OSS",
  }));
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: matrix.length, tasks: matrix } }),
  );
  for (const [index, status] of states.entries()) {
    await page.goto(`/tasks?taskId=${9000 + index}`);
    await expect(
      page.getByRole("heading", {
        name: `迁移任务 #${9000 + index}`,
        exact: true,
      }),
    ).toBeVisible();
    const button = page.getByRole("button", {
      name: "导出任务报告",
      exact: true,
    });
    const ended = ["STOPPED", "COMPLETED", "FAILED", "PARTIAL_FAILED"].includes(
      status,
    );
    await expect(button).toBeEnabled({ enabled: ended });
    if (!ended) {
      await expect(
        page.getByText("停止或结束后可导出报告。", { exact: true }),
      ).toBeVisible();
      await button.dispatchEvent("click");
    } else
      await expect(
        page.getByText("停止或结束后可导出报告。", { exact: true }),
      ).toHaveCount(0);
  }
  expect(requests.some((item) => item.path.includes("/download-ticket"))).toBe(
    false,
  );
});

test("task detail omits internal dispatch budgets and redundant execution terminology", async ({
  page,
}) => {
  await mockApi(page);
  const task = {
    ...tasks[2],
    listQpsLimit: 0,
    headQpsLimit: null,
    putQpsLimit: 50,
  };
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: 1, tasks: [task] } }),
  );
  await page.goto("/tasks?taskId=1046");
  const detail = page.locator(".MuiDrawer-paper").filter({
    has: page.getByRole("heading", { name: "迁移任务 #1046", exact: true }),
  });
  await expect(detail.getByText(/任务限额|使用节点上限|50 次\/秒/)).toHaveCount(
    0,
  );
  await expect(detail.getByText("不限", { exact: true })).toHaveCount(0);
  await expect(
    detail.getByText(
      "任务同时受节点总上限和任务限额约束；节点额度由任务共享。",
      { exact: true },
    ),
  ).toHaveCount(0);
  await expect(
    detail.getByText("分片是分配给节点执行的子任务，父任务汇总各分片进度。", {
      exact: true,
    }),
  ).toHaveCount(0);
});

test("storage synchronization presents the master and every remote result including partial failure", async ({
  page,
}) => {
  await mockApi(page);
  let saved = 0;
  await page.route("**/api/master/s3-config/production", async (route) => {
    expect(route.request().method()).toBe("PUT");
    saved++;
    await route.fulfill({
      json:
        saved === 1
          ? {
              success: true,
              partial: false,
              successCount: 2,
              failedCount: 0,
              message: "更新结果：成功 2 个节点，失败 0 个节点",
              details: ["10.0.1.12: 更新成功", "10.0.1.13: 更新成功"],
            }
          : {
              success: true,
              partial: true,
              successCount: 1,
              failedCount: 1,
              message: "更新结果：成功 1 个节点，失败 1 个节点",
              details: ["10.0.1.12: 更新成功", "10.0.1.13: 更新异常 - Timeout"],
            },
    });
  });
  await page.goto("/storage");
  for (let run = 0; run < 2; run++) {
    await page
      .getByRole("button", { name: "编辑连接 production", exact: true })
      .click();
    await page
      .getByRole("dialog")
      .getByRole("button", { name: "保存连接", exact: true })
      .click();
    await expect(page.getByRole("dialog")).toBeHidden();
    const summary = page
      .getByRole("alert")
      .filter({ has: page.getByRole("list") });
    await expect(summary.getByRole("listitem")).toHaveCount(2);
    await expect(summary).toContainText("10.0.1.12: 更新成功");
    await expect(summary).toContainText(
      run === 0 ? "10.0.1.13: 更新成功" : "10.0.1.13: 更新异常 - Timeout",
    );
    await expect(summary).toContainText(
      run === 0 ? "连接更新完成" : "部分节点更新失败",
    );
  }
});
