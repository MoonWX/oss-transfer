import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

const emptyResult = {
  success: true,
  deletedTasks: { "10.0.1.12": [], "10.0.1.13": [] },
  totalCount: 0,
  complete: true,
  partial: false,
};

test("empty recycle content stays mounted and keeps its geometry during background and manual refresh", async ({
  page,
}, testInfo) => {
  await page.setViewportSize({ width: 390, height: 640 });
  await mockApi(page);
  await page.clock.install();
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  await page.route("**/api/master/deleted-tasks", (route) =>
    route.fulfill({ json: emptyResult }),
  );
  await page.goto("/tools?tab=recycle");
  const empty = page.getByText("暂无已删除任务", { exact: true });
  await expect(empty).toBeVisible();
  await page.clock.pauseAt(await page.evaluate(() => Date.now() + 100));
  const handle = await empty.elementHandle();
  await empty.scrollIntoViewIfNeeded();
  const before = await empty.boundingBox();
  const scroll = await page.evaluate(() => window.scrollY);
  let workerCalls = 0;
  let releaseWorker!: () => void;
  const workerGate = new Promise<void>((resolve) => {
    releaseWorker = resolve;
  });
  await page.route("**/api/master/worker-status", async (route) => {
    workerCalls++;
    await workerGate;
    await route.fallback();
  });
  try {
    await page.clock.runFor(1000);
    await expect.poll(() => workerCalls).toBe(1);
    await expect(empty).toBeVisible();
    expect(await handle!.evaluate((element) => element.isConnected)).toBe(true);
    expect(await empty.boundingBox()).toEqual(before);
    expect(await page.evaluate(() => window.scrollY)).toBe(scroll);
    await page.screenshot({
      path: testInfo.outputPath("recycle-empty-mobile-refresh.png"),
      scale: "css",
    });
  } finally {
    releaseWorker();
  }
  await expect(
    page.getByText("节点清单读取失败", { exact: false }),
  ).toHaveCount(0);

  let recycleCalls = 0;
  let releaseRecycle!: () => void;
  const recycleGate = new Promise<void>((resolve) => {
    releaseRecycle = resolve;
  });
  await page.route("**/api/master/deleted-tasks", async (route) => {
    recycleCalls++;
    await recycleGate;
    await route.fulfill({ json: emptyResult });
  });
  try {
    await page
      .getByRole("button", { name: "刷新", exact: true })
      .evaluate((button) => (button as HTMLButtonElement).click());
    await expect.poll(() => recycleCalls).toBe(1);
    await expect(
      page.getByRole("progressbar", { name: "刷新回收站" }),
    ).toBeVisible();
    expect(await handle!.evaluate((element) => element.isConnected)).toBe(true);
    await expect(empty).toBeVisible();
    expect(await empty.boundingBox()).toEqual(before);
    expect(await page.evaluate(() => window.scrollY)).toBe(scroll);
  } finally {
    releaseRecycle();
  }
  await expect(
    page.getByRole("button", { name: "刷新", exact: true }),
  ).toBeEnabled();
  expect(await empty.boundingBox()).toEqual(before);
  expect(await handle!.evaluate((element) => element.isConnected)).toBe(true);
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(390);
});

test("reordered recycle responses preserve row identity selection pagination and scroll", async ({
  page,
}, testInfo) => {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "0"),
  );
  const records = Array.from({ length: 40 }, (_, index) => ({
    taskIndex: index + 1,
    sourceBucket: "source",
    destBucket: "destination",
    status: "COMPLETED",
    putObjectCount: 1,
  }));
  let reverse = false;
  let release!: () => void;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route("**/api/master/deleted-tasks", async (route) => {
    if (reverse) await gate;
    await route.fulfill({
      json: {
        ...emptyResult,
        totalCount: records.length,
        deletedTasks: {
          "10.0.1.12": reverse ? [...records].reverse() : records,
          "10.0.1.13": [],
        },
      },
    });
  });
  await page.goto("/tools?tab=recycle");
  await page.getByRole("button", { name: "下一页", exact: true }).click();
  const selected = page.getByRole("checkbox", {
    name: "选择任务 15",
    exact: true,
  });
  await selected.check();
  const handle = await selected.elementHandle();
  await selected.scrollIntoViewIfNeeded();
  const before = await selected.boundingBox();
  const scroll = await page.evaluate(() => window.scrollY);
  reverse = true;
  try {
    await page
      .getByRole("button", { name: "刷新", exact: true })
      .evaluate((button) => (button as HTMLButtonElement).click());
    await expect(
      page.getByRole("progressbar", { name: "刷新回收站" }),
    ).toBeVisible();
    expect(await handle!.evaluate((element) => element.isConnected)).toBe(true);
    await expect(selected).toBeChecked();
    expect(await selected.boundingBox()).toEqual(before);
    await page.screenshot({
      path: testInfo.outputPath("recycle-selected-page-refresh.png"),
      scale: "css",
    });
  } finally {
    release();
  }
  await expect(
    page.getByRole("button", { name: "刷新", exact: true }),
  ).toBeEnabled();
  await expect(selected).toBeChecked();
  expect(await handle!.evaluate((element) => element.isConnected)).toBe(true);
  expect(await selected.boundingBox()).toEqual(before);
  expect(await page.evaluate(() => window.scrollY)).toBe(scroll);
  await expect(page.getByText("26–40 / 40", { exact: true })).toBeVisible();
});
