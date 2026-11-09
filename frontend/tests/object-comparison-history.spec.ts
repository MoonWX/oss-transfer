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

/** Waits until the page has received every reply it asked for so far. */
async function settle(page: Page, calls: Call[]) {
  await Promise.all(live(calls).map((call) => call.request.response()));
  await page.evaluate(() => undefined);
}

const unavailableItem = (extra: Record<string, unknown> = {}) => ({
  id: "comparison-1",
  status: "UNAVAILABLE",
  mode: "metadata",
  scope: "prefix",
  source: progress.source,
  destination: progress.destination,
  sourcePrefix: progress.sourcePrefix,
  destinationPrefix: progress.destinationPrefix,
  compared: 0,
  matched: 0,
  startedAt: progress.startedAt,
  nodeId: "node-a",
  nodeName: "agent-a",
  ...extra,
});
const unreachableList = (extra: Record<string, unknown> = {}) => ({
  items: [unavailableItem(extra)],
  complete: false,
  unavailableNodes: [{ nodeId: "node-a", nodeName: "agent-a", count: 1 }],
});
const unreachableBanner =
  "1 个节点暂时无法连接，这些节点上的比对未列出最新状态。";
const nodeDownText = "执行节点暂时无法连接，节点恢复后将自动加载比对结果。";

test("an unavailable node shows a localized state and is retried with growing delays", async ({
  page,
}) => {
  await mockApi(page);
  await installPausedClock(page);
  let back = false;
  const calls = await routeComparisons(page, {
    list: () =>
      back ? { items: [historyItem()], complete: true } : unreachableList(),
    page: (_, query) =>
      back
        ? pageOf(progress, rows, query)
        : { status: 503, json: { success: false, message: "节点离线" } },
  });
  await page.goto("/tools");
  await advanceUntilVisible(page, page.getByText(unreachableBanner));
  await page
    .getByRole("button", {
      name: "source/photos/2026/ → target/archive/backup/",
    })
    .click();
  const result = page.getByRole("region", { name: "比对结果" });
  await expect(result.getByText(nodeDownText, { exact: true })).toBeVisible();
  await expect(result.getByText("节点离线")).toHaveCount(0);
  await expect.poll(() => pageQueries(calls).length).toBe(1);
  await settle(page, calls);
  // Retries after 5, 10, 20, 40 and then every 60 seconds.
  for (const [wait, reads] of [
    [5000, 2],
    [10000, 3],
    [20000, 4],
    [40000, 5],
    [60000, 6],
    [60000, 7],
  ] as const) {
    await page.clock.runFor(wait - 1);
    expect(pageQueries(calls)).toHaveLength(reads - 1);
    await page.clock.runFor(1);
    await expect.poll(() => pageQueries(calls).length).toBe(reads);
    await settle(page, calls);
  }
  await expect(result.getByText(nodeDownText, { exact: true })).toBeVisible();
  // The node is back: the next slow history refresh reloads the panel.
  back = true;
  await page.clock.runFor(15000);
  await expect(result.getByRole("heading", { name: "比对完成" })).toBeVisible();
  await expect(result.getByText(nodeDownText, { exact: true })).toHaveCount(0);
  await expect(page.getByText(unreachableBanner)).toHaveCount(0);
});

test("raw load errors in the result panel are translated", async ({ page }) => {
  await mockApi(page);
  await routeComparisons(page, {
    list: () => ({ items: [historyItem()], complete: true }),
    page: () => ({
      status: 502,
      json: { success: false, message: "context deadline exceeded" },
    }),
  });
  await page.goto("/tools?comparison=comparison-1");
  const result = page.getByRole("region", { name: "比对结果" });
  await expect(result.getByText("请求超时", { exact: true })).toBeVisible();
  await expect(result.getByText("context deadline exceeded")).toHaveCount(0);
});

test("a first load that differs from the history refreshes the history once", async ({
  page,
}) => {
  await mockApi(page);
  await installPausedClock(page);
  let back = false;
  const calls = await routeComparisons(page, {
    list: () =>
      back ? { items: [historyItem()], complete: true } : unreachableList(),
    page: (_, query) => pageOf(progress, rows, query),
  });
  await page.goto("/tools");
  await advanceUntilVisible(page, page.getByText(unreachableBanner));
  const lists = () => live(calls, "list").length;
  const first = lists();
  const started = await page.evaluate(() => Date.now());
  back = true;
  await page
    .getByRole("button", {
      name: "source/photos/2026/ → target/archive/backup/",
    })
    .click();
  const result = page.getByRole("region", { name: "比对结果" });
  await expect(result.getByRole("heading", { name: "比对完成" })).toBeVisible();
  await expect.poll(lists).toBe(first + 1);
  const history = page.getByRole("table", { name: "比对历史" });
  await expect(history.getByRole("cell").nth(0)).toHaveText("比对完成");
  await expect(page.getByText(unreachableBanner)).toHaveCount(0);
  // The refresh came from the panel, not from the slow history poll.
  expect(await page.evaluate(() => Date.now())).toBeLessThan(started + 15000);
  // Once both agree, nothing else is requested.
  await page.clock.runFor(30000);
  expect(lists()).toBe(first + 1);
});

test("history refreshes every 15 seconds while a node is unreachable", async ({
  page,
}) => {
  await mockApi(page);
  await installPausedClock(page);
  let back = false;
  const calls = await routeComparisons(page, {
    list: () =>
      back
        ? { items: [historyItem({ status: "COMPLETED" })], complete: true }
        : unreachableList(),
  });
  await page.goto("/tools");
  await advanceUntilVisible(page, page.getByText(unreachableBanner));
  const lists = () => live(calls, "list").length;
  const first = lists();
  // The next refresh was scheduled less than 15 seconds ago.
  await page.clock.runFor(15000);
  await expect.poll(lists).toBe(first + 1);
  const row = page
    .getByRole("table", { name: "比对历史" })
    .getByRole("row")
    .filter({ hasText: "source/photos/2026/" });
  await expect(row.getByRole("cell").nth(0)).toHaveText("节点不可用");
  back = true;
  await page.clock.runFor(15000);
  await expect.poll(lists).toBe(first + 2);
  await expect(row.getByRole("cell").nth(0)).toHaveText("比对完成");
  await expect(page.getByText(unreachableBanner)).toHaveCount(0);
  // Every node answered and nothing runs: polling stops.
  await page.clock.runFor(60000);
  expect(lists()).toBe(first + 2);
});

test("a stalled filtered scan keeps refreshing the progress of a running comparison", async ({
  page,
}) => {
  await mockApi(page);
  await installPausedClock(page);
  let status = "RUNNING";
  let compared = 1000;
  // Every page read advances the comparison by 100 objects at 100 per second.
  const current = () => ({
    ...large,
    status,
    bothMissing: 1,
    compared,
    elapsedMillis: compared * 10,
  });
  const calls = await routeComparisons(page, {
    list: () => ({ items: [historyItem(current())], complete: true }),
    page: (_, query) => {
      if (status === "RUNNING") compared += 100;
      if (query.get("status") === "BOTH_MISSING") {
        const after = Number(query.get("after"));
        return { ...current(), items: [], nextAfter: after + 1000 };
      }
      return pageOf(current(), many, query);
    },
  });
  await page.goto("/tools?comparison=comparison-1");
  await advanceUntilVisible(page, keyCell(page, 1));
  await page.getByRole("button", { name: "两端均不存在 1" }).click();
  const keepSearching = page.getByRole("button", { name: "继续查找" });
  await expect(keepSearching).toBeVisible();
  const stalledText = page.getByText(
    "已查找到第 6,000 条，暂未找到符合条件的差异。",
    { exact: true },
  );
  await expect(stalledText).toBeVisible();
  const filtered = () =>
    pageQueries(calls).filter((query) => query.includes("status=")).length;
  const progressReads = () =>
    pageQueries(calls).filter((query) => query === "after=0&limit=1").length;
  const scanned = filtered();
  expect(scanned).toBe(6);
  await settle(page, calls);
  const stats = (value: number) =>
    page.getByText(
      `已检查 ${value.toLocaleString("en-US")}，100 个/秒，已用 ${value / 100} 秒`,
      { exact: true },
    );
  for (const reads of [1, 2]) {
    await page.clock.runFor(1500);
    await expect.poll(progressReads).toBe(reads);
    await expect(stats(compared)).toBeVisible();
    await settle(page, calls);
  }
  expect(filtered()).toBe(scanned);
  await expect(stalledText).toBeVisible();
  await expect(keepSearching).toBeVisible();
  await expect(
    page.getByRole("button", { name: "两端均不存在 1" }),
  ).toHaveAttribute("aria-pressed", "true");
  // Once the comparison ends, the progress refresh stops.
  status = "COMPLETED";
  await page.clock.runFor(1500);
  await expect.poll(progressReads).toBe(3);
  await expect(page.getByRole("heading", { name: "比对完成" })).toBeVisible();
  await page.clock.runFor(15000);
  expect(progressReads()).toBe(3);
  expect(filtered()).toBe(scanned);
  await expect(stalledText).toBeVisible();
});
