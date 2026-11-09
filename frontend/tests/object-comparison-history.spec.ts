import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";
import { advanceUntilVisible, installPausedClock } from "./clock";
import {
  historyItem,
  live,
  pageOf,
  progress,
  routeComparisons,
  row,
  rows,
  type Call,
} from "./object-comparison-fixtures";

// 250 stored differences: every fifth is missing on the target.
const many = Array.from({ length: 250 }, (_, index) =>
  row(index + 1, (index + 1) % 5 === 0 ? "MISSING_TARGET" : "DIFFERENT"),
);
const large = {
  ...progress,
  compared: 1000,
  matched: 750,
  different: 200,
  etagMismatch: 0,
  missing: 50,
  extra: 0,
  rows: 250,
};
const pageQueries = (calls: Call[]) =>
  live(calls, "page").map((call) => call.query.toString());
const keyCell = (page: Page, number: number) =>
  page.getByRole("cell", {
    name: `2026/file-${String(number).padStart(4, "0")}.bin`,
    exact: true,
  });

test("history lists comparisons from every node, flags unreachable nodes and selects by URL", async ({
  page,
}) => {
  await mockApi(page);
  const running = historyItem({
    id: "comparison-2",
    status: "RUNNING",
    nodeId: "node-b",
    nodeName: "agent-b",
    sourcePrefix: "2027/",
    compared: 500,
    matched: 480,
    startedAt: "2026-09-22T11:30:00Z",
  });
  const unavailable = {
    id: "comparison-3",
    status: "UNAVAILABLE",
    mode: "content",
    scope: "prefix",
    source: { kind: "OSS", ref: "source", bucket: "photos" },
    destination: { kind: "OSS", ref: "target", bucket: "archive" },
    sourcePrefix: "2025/",
    destinationPrefix: "old/",
    compared: 0,
    matched: 0,
    startedAt: "0001-01-01T00:00:00Z",
    nodeId: "node-c",
    nodeName: "agent-c",
  };
  await routeComparisons(page, {
    list: () => ({
      items: [running, historyItem(), unavailable],
      complete: false,
      unavailableNodes: [{ nodeId: "node-c", nodeName: "agent-c", count: 1 }],
    }),
    page: (id, query) =>
      pageOf(
        id === "comparison-2"
          ? { ...progress, ...running, nodeId: undefined, nodeName: undefined }
          : progress,
        rows,
        query,
      ),
  });
  await page.goto("/tools");
  const history = page.getByRole("table", { name: "比对历史" });
  await expect(
    page.getByText("1 个节点暂时无法连接，这些节点上的比对未列出最新状态。", {
      exact: true,
    }),
  ).toBeVisible();
  const runningRow = history
    .getByRole("row")
    .filter({ hasText: "source/photos/2027/" });
  await expect(runningRow.getByRole("cell").nth(0)).toHaveText("正在比对");
  await expect(runningRow.getByRole("cell").nth(2)).toHaveText("元数据");
  await expect(runningRow.getByRole("cell").nth(3)).toHaveText("500");
  await expect(runningRow.getByRole("cell").nth(4)).toHaveText("20");
  await expect(runningRow.getByRole("cell").nth(6)).toHaveText("agent-b");
  const unavailableRow = history
    .getByRole("row")
    .filter({ hasText: "source/photos/2025/" });
  await expect(unavailableRow.getByRole("cell").nth(0)).toHaveText(
    "节点不可用",
  );
  await expect(unavailableRow.getByRole("cell").nth(2)).toHaveText("SHA-256");
  await expect(unavailableRow.getByRole("cell").nth(3)).toHaveText("—");
  await expect(unavailableRow.getByRole("cell").nth(4)).toHaveText("—");
  await expect(unavailableRow.getByRole("cell").nth(5)).toHaveText("—");
  const completed = history.getByRole("button", {
    name: "source/photos/2026/ → target/archive/backup/",
  });
  await completed.click();
  await expect(page).toHaveURL(/[?&]comparison=comparison-1(&|$)/);
  await expect(page.getByRole("heading", { name: "比对完成" })).toBeVisible();
  await expect(completed).toHaveAttribute("aria-current", "true");
  const result = page.getByRole("region", { name: "比对结果" });
  await expect(result).toBeFocused();
  await expect(result.getByText("agent-a", { exact: true })).toBeVisible();
  // Keyboard selection of another comparison.
  await history
    .getByRole("button", {
      name: "source/photos/2027/ → target/archive/backup/",
    })
    .focus();
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/[?&]comparison=comparison-2(&|$)/);
  await expect(page.getByRole("heading", { name: "正在比对" })).toBeVisible();
  await expect(completed).not.toHaveAttribute("aria-current", "true");
  await page.goBack();
  await expect(page.getByRole("heading", { name: "比对完成" })).toBeVisible();
});

test("history refreshes every 5 seconds only while a comparison is running", async ({
  page,
}) => {
  await mockApi(page);
  await installPausedClock(page);
  let status = "RUNNING";
  const calls = await routeComparisons(page, {
    list: () => ({ items: [historyItem({ status })], complete: true }),
  });
  await page.goto("/tools");
  await advanceUntilVisible(page, page.getByText("正在比对", { exact: true }));
  const lists = () => live(calls, "list").length;
  const first = lists();
  // The next refresh was scheduled less than 5 seconds ago (it has not run).
  await page.clock.runFor(5000);
  await expect.poll(lists).toBe(first + 1);
  status = "COMPLETED";
  await page.clock.runFor(5000);
  await expect(page.getByText("比对完成", { exact: true })).toBeVisible();
  const settled = lists();
  await page.clock.runFor(20000);
  expect(lists()).toBe(settled);
  await page.getByRole("button", { name: "刷新" }).click();
  await expect.poll(lists).toBe(settled + 1);
});

test("status chips filter server pages and reset paging", async ({ page }) => {
  await mockApi(page);
  const calls = await routeComparisons(page, {
    list: () => ({ items: [historyItem(large)], complete: true }),
    page: (_, query) => pageOf(large, many, query),
  });
  await page.goto("/tools?comparison=comparison-1");
  await expect(keyCell(page, 100)).toBeVisible();
  await page.getByRole("button", { name: "下一页" }).click();
  await expect(keyCell(page, 101)).toBeVisible();
  await expect(page.getByText("第 2 页", { exact: true })).toBeVisible();
  const chips = page.getByRole("group", { name: "按结果筛选" });
  const missing = chips.getByRole("button", { name: "目标缺失 50" });
  // Chips are keyboard operable toggle buttons.
  await missing.focus();
  await page.keyboard.press("Enter");
  await expect(missing).toHaveAttribute("aria-pressed", "true");
  await expect(keyCell(page, 5)).toBeVisible();
  await expect(keyCell(page, 250)).toBeVisible();
  await expect(keyCell(page, 101)).toHaveCount(0);
  await expect(
    page.getByRole("navigation", { name: "差异明细分页" }),
  ).toHaveCount(0);
  await chips.getByRole("button", { name: "全部差异 250" }).click();
  await expect(keyCell(page, 1)).toBeVisible();
  await expect(missing).toHaveAttribute("aria-pressed", "false");
  expect(pageQueries(calls)).toEqual([
    "after=0&limit=100",
    "after=100&limit=100",
    "after=0&limit=100&status=MISSING_TARGET",
    "after=0&limit=100",
  ]);
});

test("cursor paging moves forward and back, and polling keeps the current page", async ({
  page,
}) => {
  await mockApi(page);
  await installPausedClock(page);
  const running = { ...large, status: "RUNNING" };
  const calls = await routeComparisons(page, {
    list: () => ({ items: [historyItem(running)], complete: true }),
    page: (_, query) => pageOf(running, many, query),
  });
  await page.goto("/tools?comparison=comparison-1");
  await advanceUntilVisible(page, keyCell(page, 100));
  // Earlier polls of the first page may have run while the shell started.
  const baseline = pageQueries(calls).length;
  const next = page.getByRole("button", { name: "下一页" });
  const previous = page.getByRole("button", { name: "上一页" });
  await expect(previous).toBeDisabled();
  await next.click();
  await expect(keyCell(page, 200)).toBeVisible();
  await next.click();
  await expect(keyCell(page, 250)).toBeVisible();
  await expect(page.getByText("第 3 页", { exact: true })).toBeVisible();
  await expect(next).toBeDisabled();
  await previous.click();
  await expect(keyCell(page, 101)).toBeVisible();
  await expect(page.getByText("第 2 页", { exact: true })).toBeVisible();
  const before = pageQueries(calls).length;
  await page.clock.runFor(1500);
  await expect.poll(() => pageQueries(calls).length).toBe(before + 1);
  await page.clock.runFor(1500);
  await expect.poll(() => pageQueries(calls).length).toBe(before + 2);
  await expect(keyCell(page, 101)).toBeVisible();
  await expect(keyCell(page, 1)).toHaveCount(0);
  await expect(page.getByText("第 2 页", { exact: true })).toBeVisible();
  expect(pageQueries(calls).slice(baseline)).toEqual([
    "after=100&limit=100",
    "after=200&limit=100",
    "after=100&limit=100",
    "after=100&limit=100",
    "after=100&limit=100",
  ]);
});

test("an empty filtered page continues automatically five times, then offers to keep searching", async ({
  page,
}) => {
  await mockApi(page);
  const sparse = { ...large, changed: 1 };
  const calls = await routeComparisons(page, {
    list: () => ({ items: [historyItem(sparse)], complete: true }),
    page: (_, query) => {
      const after = Number(query.get("after"));
      if (query.get("status") !== "CHANGED_DURING_COMPARE")
        return pageOf(sparse, many, query);
      // The Master's scan budget ends before a matching row is reached.
      if (after < 6000)
        return { ...sparse, items: [], nextAfter: after + 1000 };
      return {
        ...sparse,
        items: [row(6500, "CHANGED_DURING_COMPARE")],
        nextAfter: 0,
      };
    },
  });
  await page.goto("/tools?comparison=comparison-1");
  await expect(keyCell(page, 1)).toBeVisible();
  await page.getByRole("button", { name: "比对期间已变化 1" }).click();
  const keepSearching = page.getByRole("button", { name: "继续查找" });
  await expect(keepSearching).toBeVisible();
  await expect(
    page.getByText("已查找到第 6,000 条，暂未找到符合条件的差异。", {
      exact: true,
    }),
  ).toBeVisible();
  expect(pageQueries(calls).slice(1)).toEqual(
    [0, 1000, 2000, 3000, 4000, 5000].map(
      (after) => `after=${after}&limit=100&status=CHANGED_DURING_COMPARE`,
    ),
  );
  await keepSearching.click();
  await expect(keyCell(page, 6500)).toBeVisible();
  await expect(keepSearching).toHaveCount(0);
  expect(pageQueries(calls).at(-1)).toBe(
    "after=6000&limit=100&status=CHANGED_DURING_COMPARE",
  );
});

test("a comparison that disappears while polling stops polling and says so", async ({
  page,
}) => {
  await mockApi(page);
  await installPausedClock(page);
  let gone = false;
  const calls = await routeComparisons(page, {
    list: () => ({
      items: gone ? [] : [historyItem({ status: "RUNNING" })],
      complete: true,
    }),
    page: (_, query) =>
      gone
        ? {
            status: 404,
            json: { success: false, message: "比对记录不存在" },
          }
        : pageOf({ ...progress, status: "RUNNING" }, rows, query),
  });
  await page.goto("/tools?comparison=comparison-1");
  await advanceUntilVisible(
    page,
    page.getByRole("heading", { name: "正在比对" }),
  );
  gone = true;
  await page.clock.runFor(1500);
  await expect(
    page.getByText("比对结果不存在或已被删除。", { exact: true }),
  ).toBeVisible();
  await expect(page.getByRole("progressbar", { name: "比对进度" })).toHaveCount(
    0,
  );
  await expect(page.getByRole("button", { name: "导出 ZIP" })).toHaveCount(0);
  const reads = pageQueries(calls).length;
  await page.clock.runFor(20000);
  expect(pageQueries(calls)).toHaveLength(reads);
  // The Master forgets the assignment, so the history is refreshed too.
  await expect(page.getByText("暂无比对记录", { exact: true })).toBeVisible();
});

for (const theme of ["light", "dark"] as const)
  test(`phone width keeps history, filters and differences inside the viewport (${theme})`, async ({
    page,
  }, testInfo) => {
    await page.setViewportSize({ width: 360, height: 800 });
    await mockApi(page);
    await page.addInitScript(
      (value) => localStorage.setItem("oss-transfer-theme", value),
      theme,
    );
    const content = {
      ...progress,
      mode: "content",
      status: "RUNNING",
      bytesRead: 5 * 1024 ** 3,
      elapsedMillis: 3_725_000,
      rowsTruncated: true,
      truncatedReason: "STORAGE_BUDGET",
      lastStoredKey: `very/long/${"segment/".repeat(12)}key.bin`,
    };
    const longRows = [
      row(1, "DIFFERENT", {
        sourceKey: `2026/${"deep/".repeat(20)}object-with-a-long-name.bin`,
        destinationKey: `backup/${"deep/".repeat(20)}object-with-a-long-name.bin`,
        reasons: ["sha256"],
        sourceSha256: "a".repeat(64),
        destinationSha256: "b".repeat(64),
      }),
      row(2, "ERROR", {
        error: "无法完整读取对象内容",
        errorDetail: "target GET: timeout",
      }),
    ];
    await routeComparisons(page, {
      list: () => ({
        items: [historyItem(content), historyItem({ id: "comparison-2" })],
        complete: true,
      }),
      page: (_, query) => pageOf(content, longRows, query),
    });
    await page.goto("/tools?comparison=comparison-1");
    await expect(
      page.getByText(/^已检查 10，.* 个\/秒，已用 1 小时 2 分，已读取 5 GiB$/),
    ).toBeVisible();
    await expect(
      page.getByRole("button", { name: /source\/photos\/2026\// }).first(),
    ).toBeVisible();
    await expect(
      page.getByText("target GET: timeout", { exact: true }),
    ).toBeVisible();
    await expect(page.getByText("a".repeat(64), { exact: true })).toBeVisible();
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth),
    ).toBeLessThanOrEqual(360);
    await page.screenshot({
      path: testInfo.outputPath(`comparison-mobile-${theme}.png`),
      fullPage: true,
    });
  });

test("desktop layout pages a long history and keeps the result inside the viewport", async ({
  page,
}, testInfo) => {
  await page.setViewportSize({ width: 1280, height: 1400 });
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (message) => {
    if (message.type() === "error") errors.push(message.text());
  });
  await mockApi(page);
  const interrupted = {
    ...large,
    status: "INTERRUPTED",
    resumable: true,
    elapsedMillis: 135_000,
    errorDetail: "storage connections changed; resume manually",
  };
  // Twelve comparisons, newest first; the oldest two are on the second page.
  const items = [
    historyItem(interrupted),
    ...Array.from({ length: 11 }, (_, index) =>
      historyItem({
        id: `comparison-${index + 2}`,
        status: index % 3 === 0 ? "FAILED" : "COMPLETED",
        sourcePrefix: `${2025 - index}/`,
      }),
    ),
  ];
  await routeComparisons(page, {
    list: () => ({ items, complete: true }),
    page: (_, query) =>
      pageOf(interrupted, [rows[0], rows[1], ...many.slice(2, 12)], query),
  });
  await page.goto("/tools?comparison=comparison-1");
  await expect(page.getByRole("button", { name: "继续比对" })).toBeVisible();
  await expect(
    page.getByText("已检查 1,000，7.4 个/秒，已用 2 分 15 秒", { exact: true }),
  ).toBeVisible();
  const history = page.getByRole("table", { name: "比对历史" });
  await expect(history.getByRole("row")).toHaveCount(11);
  const pages = page.getByRole("navigation", { name: "比对历史分页" });
  await pages.getByRole("button", { name: "第 2 页" }).click();
  await expect(history.getByRole("row")).toHaveCount(3);
  await expect(
    history.getByRole("button", { name: /source\/photos\/2015\// }),
  ).toBeVisible();
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(1280);
  // Narrower content switches both tables to stacked items in place.
  await page.setViewportSize({ width: 1000, height: 1400 });
  await expect(page.getByRole("list", { name: "比对历史" })).toBeVisible();
  await expect(page.getByRole("list", { name: "差异明细" })).toBeVisible();
  await expect(page.getByRole("table")).toHaveCount(0);
  await page.setViewportSize({ width: 1280, height: 1400 });
  await expect(history).toBeVisible();
  await expect(page.getByRole("table", { name: "差异明细" })).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("comparison-desktop.png"),
    fullPage: true,
  });
  expect(errors).toEqual([]);
});

test("empty result pages say whether nothing differs, nothing matches or nothing was stored", async ({
  page,
}) => {
  await mockApi(page);
  let current: Record<string, unknown> = {
    ...progress,
    compared: 5,
    matched: 5,
    different: 0,
    etagMismatch: 0,
    missing: 0,
    extra: 0,
    rows: 0,
  };
  await routeComparisons(page, {
    list: () => ({ items: [historyItem(current)], complete: true }),
    page: (_, query) => pageOf(current, [], query),
  });
  await page.goto("/tools?comparison=comparison-1");
  await expect(page.getByText("未发现差异。", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "目标缺失 0" }).click();
  await expect(
    page.getByText("没有符合条件的差异。", { exact: true }),
  ).toBeVisible();
  current = {
    ...current,
    matched: 0,
    missing: 5,
    rowsTruncated: true,
    truncatedReason: "LOW_DISK_SPACE",
  };
  await page.getByRole("button", { name: "刷新" }).click();
  await page.getByRole("button", { name: "全部差异 0" }).click();
  await expect(
    page.getByText("暂无已保存的差异明细。", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText(
      "节点磁盘剩余空间不足，之后的差异只计数未保存。最后保存的 Key：—",
      { exact: true },
    ),
  ).toBeVisible();
});

test("a state change seen by the history poll reloads the selected comparison", async ({
  page,
}) => {
  await mockApi(page);
  await installPausedClock(page);
  // Resumed elsewhere, for example with "object resume" in the CLI.
  let selected = "INTERRUPTED";
  const other = historyItem({
    id: "comparison-2",
    status: "RUNNING",
    sourcePrefix: "2027/",
  });
  const calls = await routeComparisons(page, {
    list: () => ({
      items: [other, historyItem({ status: selected })],
      complete: true,
    }),
    page: (id, query) =>
      pageOf(
        id === "comparison-2" ? other : { ...progress, status: selected },
        rows,
        query,
      ),
  });
  await page.goto("/tools?comparison=comparison-1");
  await advanceUntilVisible(
    page,
    page.getByRole("heading", { name: "比对已中断" }),
  );
  const reads = pageQueries(calls).length;
  selected = "RUNNING";
  // The other running comparison keeps the history refreshing every 5 s.
  await page.clock.runFor(5000);
  await expect(page.getByRole("heading", { name: "正在比对" })).toBeVisible();
  expect(pageQueries(calls).length).toBeGreaterThan(reads);
  await expect(page.getByRole("button", { name: "取消比对" })).toBeVisible();
});
