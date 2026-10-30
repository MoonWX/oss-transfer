import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

const record = (key: string) => ({
  taskIndex: 1,
  key,
  sourceBucket: "source",
  destBucket: "destination",
  errorMessage: "Fixture transfer failure",
});

async function watchEmptyFlashes(page: Page) {
  await page.evaluate(() => {
    const flashes: string[] = [];
    Reflect.set(window, "exceptionEmptyFlashes", flashes);
    new MutationObserver((changes) => {
      for (const change of changes) {
        for (const node of change.addedNodes) {
          if (node.textContent?.includes("暂无匹配记录"))
            flashes.push(node.textContent);
        }
      }
    }).observe(document.getElementById("main-content")!, {
      childList: true,
      subtree: true,
    });
  });
}

test("exception tabs keep their own cached rows and ignore late responses without empty flashes", async ({
  page,
}) => {
  await mockApi(page);
  const failedRefresh = deferred();
  const fallbackLoad = deferred();
  let failedRequests = 0;
  let fallbackRequests = 0;
  await page.route("**/api/master/failed", async (route) => {
    const request = ++failedRequests;
    if (request > 1) await failedRefresh.promise;
    await route.fulfill({
      json: {
        success: true,
        queriedWorkers: ["10.0.1.12"],
        failedKeys: {
          "10.0.1.12": [record(request === 1 ? "failed.txt" : "refreshed.txt")],
          "10.0.1.12Count": 1,
        },
      },
    });
  });
  await page.route("**/api/master/fallback", async (route) => {
    fallbackRequests++;
    await fallbackLoad.promise;
    await route.fulfill({
      json: {
        success: true,
        queriedWorkers: ["10.0.1.12"],
        fallbackObjects: {
          "10.0.1.12": [record("fallback.txt")],
          "10.0.1.12Count": 1,
        },
      },
    });
  });
  await page.goto("/exceptions");
  await expect(page.getByText("failed.txt", { exact: true })).toBeVisible();
  await watchEmptyFlashes(page);

  await page.getByRole("tab", { name: "降级迁移记录", exact: true }).click();
  await expect.poll(() => fallbackRequests).toBe(1);
  await expect(
    page.getByRole("region", { name: "降级迁移记录" }),
  ).toHaveAttribute("aria-busy", "true");
  await expect(page.getByText("failed.txt", { exact: true })).toHaveCount(0);
  await expect(page.getByText("暂无匹配记录", { exact: true })).toHaveCount(0);

  await page.getByRole("tab", { name: "失败对象", exact: true }).click();
  await expect.poll(() => failedRequests).toBe(2);
  await expect(page.getByRole("region", { name: "失败对象" })).toHaveAttribute(
    "aria-busy",
    "true",
  );
  await expect(page.getByText("failed.txt", { exact: true })).toBeVisible();
  fallbackLoad.resolve();
  failedRefresh.resolve();
  await expect(page.getByText("refreshed.txt", { exact: true })).toBeVisible();
  await expect(page.getByText("fallback.txt", { exact: true })).toHaveCount(0);
  await page.getByRole("tab", { name: "降级迁移记录", exact: true }).click();
  await expect(page.getByText("fallback.txt", { exact: true })).toBeVisible();
  expect(
    await page.evaluate(() => Reflect.get(window, "exceptionEmptyFlashes")),
  ).toEqual([]);
});

test("exception filters isolate rows and failed refreshes retain only matching cached data", async ({
  page,
}) => {
  await mockApi(page);
  const filteredLoad = deferred();
  let filteredRequests = 0;
  let failRefresh = false;
  await page.route("**/api/master/failed", async (route) => {
    const filtered = route.request().postDataJSON().taskIndex === 42;
    if (filtered) {
      filteredRequests++;
      await filteredLoad.promise;
    }
    if (failRefresh) {
      await route.fulfill({
        status: 503,
        json: {
          success: false,
          message: "Fixture records temporarily unavailable",
        },
      });
      return;
    }
    await route.fulfill({
      json: {
        success: true,
        queriedWorkers: ["10.0.1.12"],
        failedKeys: {
          "10.0.1.12": [record(filtered ? "task-42.txt" : "all-tasks.txt")],
          "10.0.1.12Count": 1,
        },
      },
    });
  });
  await page.goto("/exceptions");
  await expect(page.getByText("all-tasks.txt", { exact: true })).toBeVisible();
  await watchEmptyFlashes(page);
  await page
    .getByRole("combobox", { name: "所属任务", exact: true })
    .fill("42");
  await page.getByRole("button", { name: "应用筛选", exact: true }).click();
  await expect.poll(() => filteredRequests).toBe(1);
  await expect(page.getByText("all-tasks.txt", { exact: true })).toHaveCount(0);
  await expect(page.getByText("暂无匹配记录", { exact: true })).toHaveCount(0);
  filteredLoad.resolve();
  await expect(page.getByText("task-42.txt", { exact: true })).toBeVisible();
  failRefresh = true;
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText(
    "Fixture records temporarily unavailable",
  );
  await expect(page.getByText("task-42.txt", { exact: true })).toBeVisible();
  await expect(page.getByText("all-tasks.txt", { exact: true })).toHaveCount(0);
  expect(
    await page.evaluate(() => Reflect.get(window, "exceptionEmptyFlashes")),
  ).toEqual([]);
});

test("an empty local tab keeps its loaded empty state during refresh", async ({
  page,
}) => {
  await mockApi(page);
  const refresh = deferred();
  let requests = 0;
  await page.route(
    "**/api/master/local-transfers/failures?**",
    async (route) => {
      if (++requests > 1) await refresh.promise;
      await route.fulfill({
        json: { success: true, data: { items: [], total: 0 } },
      });
    },
  );
  await page.goto("/exceptions?type=local");
  const region = page.getByRole("region", { name: "本地迁移失败" });
  await expect(region.getByText("暂无匹配记录", { exact: true })).toBeVisible();
  const loadedHeight = (await region.boundingBox())!.height;
  await page.getByRole("tab", { name: "失败对象", exact: true }).click();
  await expect(page.getByRole("region", { name: "失败对象" })).toHaveAttribute(
    "aria-busy",
    "false",
  );
  await page.getByRole("tab", { name: "本地迁移失败", exact: true }).click();
  await expect.poll(() => requests).toBe(2);
  await expect(region).toHaveAttribute("aria-busy", "true");
  await expect(region.getByText("暂无匹配记录", { exact: true })).toBeVisible();
  expect((await region.boundingBox())!.height).toBe(loadedHeight);
  refresh.resolve();
  await expect(region).toHaveAttribute("aria-busy", "false");
  expect((await region.boundingBox())!.height).toBe(loadedHeight);
});

test("node tabs and modal scroll locking keep the same content width", async ({
  page,
}) => {
  await mockApi(page);
  await page.setViewportSize({ width: 1440, height: 1100 });
  await page.goto("/nodes");
  await expect(page.getByRole("tab", { name: "实时状态" })).toBeVisible();
  // Force a non-overlay scrollbar so macOS/headless Chrome detects the same
  // width change as browsers with a permanent scrollbar track.
  await page.addStyleTag({
    content: "::-webkit-scrollbar { width: 16px; height: 16px; }",
  });
  await expect(page.locator("html")).toHaveCSS("overflow-y", "scroll");
  const measure = () =>
    page.evaluate(() => {
      const rect = document
        .getElementById("main-content")!
        .getBoundingClientRect();
      return { x: rect.x, width: rect.width };
    });
  const initial = await measure();
  await page.getByRole("tab", { name: "限速历史" }).click();
  expect(await measure()).toEqual(initial);
  await page.getByRole("tab", { name: "实时状态" }).click();
  expect(await measure()).toEqual(initial);
  await page.getByRole("button", { name: "调整限速", exact: true }).click();
  await expect(page.getByRole("dialog")).toBeVisible();
  expect(await measure()).toEqual(initial);
  await page
    .getByRole("dialog")
    .getByRole("combobox", { name: "限制类型" })
    .click();
  await expect(page.getByRole("listbox")).toBeVisible();
  expect(await measure()).toEqual(initial);
  await page.keyboard.press("Escape");
  await page.getByRole("button", { name: "取消", exact: true }).click();
  await expect(page.getByRole("dialog")).toBeHidden();
  expect(await measure()).toEqual(initial);
  await expect(page.locator("html")).toHaveCSS("overflow-y", "scroll");
  expect(await page.evaluate(() => document.body.style.paddingRight)).toBe("");
});
