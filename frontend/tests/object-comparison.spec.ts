import { expect, test } from "@playwright/test";
import { readFile } from "node:fs/promises";
import { mockApi } from "./fixtures";
import { advanceUntilVisible, installPausedClock } from "./clock";
import { comparisonMessages } from "../src/locales/comparison";
import {
  historyItem,
  live,
  pageOf,
  progress,
  routeComparisons,
  row,
  rows,
} from "./object-comparison-fixtures";

const zip = Buffer.from("UEsFBgAAAAAAAAAAAAAAAAAAAAAAAA==", "base64");

test("metadata comparison submits exact prefixes, explains the ETag limit and restores by URL", async ({
  page,
}) => {
  await mockApi(page);
  let started = false;
  let body: unknown;
  const calls = await routeComparisons(page, {
    list: () => ({
      items: started ? [historyItem({ status: "RUNNING" })] : [],
      complete: true,
      unavailableNodes: [],
    }),
    start: (input) => {
      body = input;
      started = true;
      return { status: 202, json: { ...progress, status: "RUNNING" } };
    },
    page: (_, query) => pageOf(progress, rows, query),
  });
  await page.goto("/tools");
  await expect(page.getByText("暂无比对记录", { exact: true })).toBeVisible();
  await expect(page.getByRole("combobox", { name: "比对方式" })).toHaveText(
    "元数据（不读取内容）",
  );
  await page.getByRole("textbox", { name: "源 Bucket" }).fill("source/photos");
  await page
    .getByRole("textbox", { name: "目标 Bucket" })
    .fill("target/archive");
  await page.getByRole("textbox", { name: "源前缀" }).fill("2026/ + ");
  await page.getByRole("textbox", { name: "目标前缀" }).fill("backup/");
  await expect(
    page.getByText(/不读取对象内容。元数据一致不代表内容一致/),
  ).toBeVisible();
  await expect(
    page.getByText(/两端 ETag 都是普通 MD5 形式且不同时，结果标为“ETag 不同”/),
  ).toBeVisible();
  await page.getByRole("button", { name: "开始比对" }).click();
  await expect(page).toHaveURL(/[?&]comparison=comparison-1(&|$)/);
  await expect(page.getByRole("heading", { name: "比对完成" })).toBeVisible();
  expect(body).toEqual({
    sourceBucket: "source/photos",
    destBucket: "target/archive",
    sourcePrefix: "2026/ + ",
    destPrefix: "backup/",
    scope: "prefix",
    mode: "metadata",
  });
  await expect(
    page.getByRole("cell", { name: "目标缺失", exact: true }),
  ).toBeVisible();
  // The new comparison appears in the history without a manual refresh.
  await expect(
    page.getByRole("button", {
      name: "source/photos/2026/ → target/archive/backup/",
    }),
  ).toHaveAttribute("aria-current", "true");
  expect(live(calls, "page")[0].query.toString()).toBe("after=0&limit=100");
  await page.reload();
  await expect(
    page.getByRole("cell", { name: "2026/file-0001.bin", exact: true }),
  ).toBeVisible();
  await expect(page.getByRole("heading", { name: "比对完成" })).toBeVisible();
});

test("content mode starts a streaming SHA-256 comparison and cancellation keeps the loaded differences", async ({
  page,
}) => {
  await mockApi(page);
  let state = "RUNNING";
  let submitted: Record<string, string> = {};
  let canceled = 0;
  const content = {
    ...progress,
    mode: "content",
    scope: "object",
    sourcePrefix: "a.bin",
    destinationPrefix: "b.bin",
    compared: 1,
    matched: 0,
    different: 1,
    etagMismatch: 0,
    missing: 0,
    extra: 0,
    rows: 1,
    bytesRead: 3 * 1024 * 1024,
    elapsedMillis: 65000,
  };
  const contentRows = [
    row(1, "DIFFERENT", {
      sourceKey: "a.bin",
      destinationKey: "b.bin",
      reasons: ["sha256"],
      sourceSha256: "a".repeat(64),
      destinationSha256: "b".repeat(64),
    }),
  ];
  await routeComparisons(page, {
    list: () => ({ items: [], complete: true }),
    start: (input) => {
      submitted = input as Record<string, string>;
      return { status: 202, json: { ...content, status: state } };
    },
    page: (_, query) =>
      pageOf({ ...content, status: state }, contentRows, query),
    cancel: () => {
      canceled++;
      state = "CANCELED";
      return { ...content, status: "CANCELING" };
    },
  });
  await page.goto(
    "/tools?sourceBucket=source/photos&destBucket=target/archive&sourceKey=a.bin&destKey=b.bin",
  );
  await page.getByRole("combobox", { name: "比对方式" }).click();
  await page.getByRole("option", { name: "内容 SHA-256（流式）" }).click();
  await expect(page.getByText(/不保存对象文件到本地磁盘/)).toBeVisible();
  await page.getByRole("button", { name: "开始比对" }).click();
  await expect(page.getByRole("heading", { name: "正在比对" })).toBeVisible();
  expect(submitted.mode).toBe("content");
  expect(submitted.scope).toBe("object");
  expect(submitted.sourcePrefix).toBe("a.bin");
  await expect(
    page.getByRole("progressbar", { name: "比对进度" }),
  ).toBeVisible();
  await expect(
    page.getByText("已检查 1，0.015 个/秒，已用 1 分 5 秒，已读取 3 MiB", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(page.getByText("a".repeat(64), { exact: true })).toBeVisible();
  await expect(page.getByText("不同：SHA-256", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "取消比对" }).click();
  await expect(page.getByRole("heading", { name: "已取消" })).toBeVisible();
  expect(canceled).toBe(1);
  await expect(page.getByRole("button", { name: "取消比对" })).toHaveCount(0);
  await expect(
    page.getByRole("cell", { name: "a.bin", exact: true }),
  ).toBeVisible();
});

test("an uncertain start reply selects the comparison the Master recorded", async ({
  page,
}) => {
  await mockApi(page);
  await routeComparisons(page, {
    list: () => ({
      items: [historyItem({ status: "RUNNING" })],
      complete: true,
    }),
    start: () => ({
      status: 502,
      json: {
        success: false,
        id: "comparison-1",
        message: "节点未确认比对是否已开始",
      },
    }),
    page: (_, query) => pageOf(progress, rows, query),
  });
  await page.goto("/tools");
  await page.getByRole("textbox", { name: "源 Bucket" }).fill("source/photos");
  await page
    .getByRole("textbox", { name: "目标 Bucket" })
    .fill("target/archive");
  await page.getByRole("button", { name: "开始比对" }).click();
  await expect(
    page.getByText("节点未确认比对是否已开始", { exact: true }),
  ).toBeVisible();
  await expect(page).toHaveURL(/[?&]comparison=comparison-1(&|$)/);
  await expect(page.getByRole("heading", { name: "比对完成" })).toBeVisible();
});

test("differences show localized reasons, ETag mismatches and credential-free error details", async ({
  page,
}) => {
  await mockApi(page);
  const failed = {
    ...progress,
    status: "FAILED",
    compared: 6,
    matched: 1,
    different: 1,
    etagMismatch: 1,
    missing: 0,
    extra: 1,
    changed: 1,
    errors: 1,
    bothMissing: 0,
    rows: 5,
    error: "比对未能完成，请检查存储访问后重试。",
    errorDetail: "source LIST: HTTP 503 SlowDown (request ID 7F3A)",
  };
  await routeComparisons(page, {
    list: () => ({ items: [historyItem(failed)], complete: true }),
    page: (_, query) =>
      pageOf(
        failed,
        [
          rows[0],
          rows[1],
          rows[3],
          row(5, "ERROR", {
            error: "无法读取目标元数据",
            errorDetail: "target HEAD: HTTP 403 AccessDenied (request ID 1A2B)",
          }),
          row(6, "CHANGED_DURING_COMPARE"),
        ],
        query,
      ),
  });
  await page.goto("/tools?comparison=comparison-1");
  await expect(page.getByRole("heading", { name: "比对失败" })).toBeVisible();
  const failure = page
    .getByRole("alert")
    .filter({ hasText: "比对未能完成，请检查存储访问后重试。" });
  await expect(failure).toBeVisible();
  await expect(failure.locator("code")).toHaveText(
    "source LIST: HTTP 503 SlowDown (request ID 7F3A)",
  );
  await expect(
    page.getByText("不同：大小、Content-Type、自定义元数据", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText("ETag 不同", { exact: true })).toBeVisible();
  await expect(page.getByText("不同：ETag", { exact: true })).toBeVisible();
  await expect(
    page.getByText("无法读取目标元数据", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("target HEAD: HTTP 403 AccessDenied (request ID 1A2B)", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(page.getByText("比对期间已变化", { exact: true })).toBeVisible();
  // Read failures and concurrent changes are never shown as confirmed results.
  await expect(
    page.getByRole("cell", { name: "目标缺失", exact: true }),
  ).toHaveCount(0);
  const extra = page.getByRole("row").filter({ hasText: "2026/file-0004.bin" });
  await expect(extra.getByRole("cell").nth(3)).toHaveText("—");
  await expect(page.getByText("一致 1", { exact: true })).toBeVisible();
  await expect(
    page.getByText("已检查 6，1.5 个/秒，已用 4 秒", { exact: true }),
  ).toBeVisible();
  const chips = page.getByRole("group", { name: "按结果筛选" });
  for (const label of [
    "全部差异 5",
    "不一致 1",
    "ETag 不同 1",
    "目标缺失 0",
    "目标多余 1",
    "比对期间已变化 1",
    "读取失败 1",
    "两端均不存在 0",
  ])
    await expect(chips.getByRole("button", { name: label })).toBeVisible();
  await expect(
    chips.getByRole("button", { name: "全部差异 5" }),
  ).toHaveAttribute("aria-pressed", "true");
});

for (const [reason, expected] of [
  [
    "LEGACY_LIMIT",
    "该节点版本最多保存前 10,000 条差异，其余差异只计数。升级该节点后重新比对可保存全部差异。",
  ],
  [
    "STORAGE_BUDGET",
    "差异明细超过节点的比对结果存储预算，之后的差异只计数未保存。最后保存的 Key：file-0004.bin",
  ],
  [
    "LOW_DISK_SPACE",
    "节点磁盘剩余空间不足，之后的差异只计数未保存。最后保存的 Key：file-0004.bin",
  ],
]) {
  test(`truncation by ${reason} explains which differences were stored`, async ({
    page,
  }) => {
    await mockApi(page);
    const truncated = {
      ...progress,
      compared: 30000,
      matched: 0,
      missing: 30000,
      rows: 4,
      rowsTruncated: true,
      resultLimit: reason === "LEGACY_LIMIT" ? 10000 : 0,
      truncatedReason: reason,
      lastStoredKey: reason === "LEGACY_LIMIT" ? undefined : "file-0004.bin",
    };
    await routeComparisons(page, {
      list: () => ({ items: [historyItem(truncated)], complete: true }),
      page: (_, query) => pageOf(truncated, rows, query),
    });
    await page.goto("/tools?comparison=comparison-1");
    await expect(page.getByText(expected, { exact: true })).toBeVisible();
    await expect(
      page.getByRole("button", { name: "目标缺失 30,000" }),
    ).toBeVisible();
  });
}

test("resume continues an interrupted comparison and polling follows it to completion", async ({
  page,
}) => {
  await mockApi(page);
  await installPausedClock(page);
  let state = "INTERRUPTED";
  const interrupted = {
    ...progress,
    status: "INTERRUPTED",
    resumable: true,
    checkpoint: "file-0004.bin",
    errorDetail: "storage connections changed; resume manually",
    endedAt: undefined,
  };
  const current = () =>
    state === "INTERRUPTED"
      ? interrupted
      : {
          ...progress,
          status: state,
          compared: state === "COMPLETED" ? 20 : 12,
          resumes: 1,
        };
  const calls = await routeComparisons(page, {
    list: () => ({ items: [historyItem(current())], complete: true }),
    page: (_, query) => pageOf(current(), rows, query),
    resume: () => {
      state = "RUNNING";
      return { status: 202, json: current() };
    },
  });
  await page.goto("/tools?comparison=comparison-1");
  const resume = page.getByRole("button", { name: "继续比对" });
  await advanceUntilVisible(page, resume);
  await expect(
    page.getByText("从已比对到的位置继续，已统计的结果保留。", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("storage connections changed; resume manually", {
      exact: true,
    }),
  ).toBeVisible();
  const lists = live(calls, "list").length;
  await resume.click();
  await expect(page.getByRole("heading", { name: "正在比对" })).toBeVisible();
  await expect(resume).toHaveCount(0);
  await expect(page.getByRole("button", { name: "取消比对" })).toBeVisible();
  expect(live(calls, "resume")).toHaveLength(1);
  await expect.poll(() => live(calls, "list").length).toBeGreaterThan(lists);
  state = "COMPLETED";
  await advanceUntilVisible(
    page,
    page.getByRole("heading", { name: "比对完成" }),
  );
  await expect(page.getByText(/^已检查 20，/)).toBeVisible();
  const reads = live(calls, "page").length;
  await page.clock.runFor(10000);
  expect(live(calls, "page")).toHaveLength(reads);
});

test("resume on a node that cannot resume shows the Master's reason", async ({
  page,
}) => {
  await mockApi(page);
  const interrupted = { ...progress, status: "INTERRUPTED", resumable: true };
  await routeComparisons(page, {
    list: () => ({ items: [historyItem(interrupted)], complete: true }),
    page: (_, query) => pageOf(interrupted, rows, query),
    resume: () => ({
      status: 409,
      json: { success: false, message: "该节点已有比对正在运行" },
    }),
  });
  await page.goto("/tools?comparison=comparison-1");
  await page.getByRole("button", { name: "继续比对" }).click();
  await expect(
    page.getByText("该节点已有比对正在运行", { exact: true }),
  ).toBeVisible();
  await expect(page.getByRole("heading", { name: "比对已中断" })).toBeVisible();
});

test("export downloads the ZIP with an authenticated ticket and a native POST", async ({
  page,
}, testInfo) => {
  await mockApi(page);
  const calls = await routeComparisons(page, {
    list: () => ({ items: [historyItem()], complete: true }),
    page: (_, query) => pageOf(progress, rows, query),
    ticket: () => ({ success: true, data: { ticket: "export-ticket-1" } }),
    download: (route, id) =>
      route.fulfill({
        contentType: "application/zip",
        headers: {
          "Content-Disposition": `attachment; filename="comparison-${id}.zip"`,
        },
        body: zip,
      }),
  });
  await page.goto("/tools?comparison=comparison-1");
  const pending = page.waitForEvent("download");
  await page.getByRole("button", { name: "导出 ZIP" }).click();
  const download = await pending;
  expect(download.suggestedFilename()).toBe("comparison-comparison-1.zip");
  const saved = testInfo.outputPath("comparison.zip");
  await download.saveAs(saved);
  expect(await readFile(saved)).toEqual(zip);
  await expect(
    page.getByText("导出已请求，请查看浏览器下载列表。", { exact: true }),
  ).toBeVisible();
  const ticket = calls.find((call) => call.kind === "ticket")!;
  expect(ticket.request.method()).toBe("GET");
  expect(ticket.request.headers().authorization).toBe("Bearer test-token");
  expect([...ticket.query]).toEqual([["format", "zip"]]);
  const post = calls.find((call) => call.kind === "download")!;
  expect(post.request.method()).toBe("POST");
  expect(post.request.resourceType()).toBe("document");
  expect(post.request.headers().authorization).toBeUndefined();
  expect(new URL(post.request.url()).search).toBe("");
  expect([...new URLSearchParams(post.request.postData() || "")]).toEqual([
    ["ticket", "export-ticket-1"],
  ]);
  await expect(page.locator('form[action$="/export"]')).toHaveCount(0);
  const frame = page.locator('iframe[data-comparison-export="comparison-1"]');
  await expect(frame).toHaveCount(1);
  await expect(frame).toBeHidden();
  await expect(page).toHaveURL(/\/tools\?comparison=comparison-1$/);
});

test("export failures are reported and leave the page in place", async ({
  page,
}) => {
  await mockApi(page);
  let ticketStatus = 409;
  await routeComparisons(page, {
    list: () => ({ items: [historyItem()], complete: true }),
    page: (_, query) => pageOf(progress, rows, query),
    ticket: () =>
      ticketStatus === 200
        ? { success: true, data: { ticket: "expired" } }
        : {
            status: ticketStatus,
            json: { success: false, message: "节点暂不可达" },
          },
    download: (route) =>
      route.fulfill({
        status: 401,
        json: { success: false, message: "下载授权已失效，请重新导出" },
      }),
  });
  await page.goto("/tools?comparison=comparison-1");
  await page.getByRole("button", { name: "导出 ZIP" }).click();
  await expect(
    page.getByText("比对结果导出失败：节点暂不可达", { exact: true }),
  ).toBeVisible();
  await expect(page.locator("iframe[data-comparison-export]")).toHaveCount(0);
  ticketStatus = 200;
  await page.getByRole("button", { name: "导出 ZIP" }).click();
  await expect(
    page.getByText("比对结果导出失败：下载授权已失效，请重新导出", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(page.locator("iframe[data-comparison-export]")).toHaveCount(0);
  await expect(page.getByRole("heading", { name: "比对完成" })).toBeVisible();
  expect(
    await page.evaluate(() => sessionStorage.getItem("oss-transfer-token")),
  ).toBe("test-token");
});

test("deleting asks for confirmation, clears the selection and refreshes the history", async ({
  page,
}) => {
  await mockApi(page);
  let deleted = false;
  let deleteStatus = 409;
  const calls = await routeComparisons(page, {
    list: () => ({ items: deleted ? [] : [historyItem()], complete: true }),
    page: (_, query) => pageOf(progress, rows, query),
    remove: () => {
      if (deleteStatus !== 200)
        return {
          status: deleteStatus,
          json: { success: false, message: "比对正在运行，请先取消" },
        };
      deleted = true;
      return { success: true };
    },
  });
  await page.goto("/tools?comparison=comparison-1");
  await page.getByRole("button", { name: "删除比对结果" }).click();
  const dialog = page.getByRole("dialog", { name: "删除比对结果？" });
  await expect(dialog).toContainText("源端和目标端的对象不受影响");
  await dialog.getByRole("button", { name: "取消" }).click();
  await expect(dialog).toBeHidden();
  expect(live(calls, "remove")).toHaveLength(0);
  await page.getByRole("button", { name: "删除比对结果" }).click();
  await dialog.getByRole("button", { name: "删除", exact: true }).click();
  await expect(
    page.getByText("比对正在运行，请先取消", { exact: true }),
  ).toBeVisible();
  await expect(page).toHaveURL(/comparison=comparison-1/);
  deleteStatus = 200;
  await page.getByRole("button", { name: "删除比对结果" }).click();
  await dialog.getByRole("button", { name: "删除", exact: true }).click();
  await expect(page).toHaveURL(/\/tools$/);
  await expect(
    page.getByText("比对结果已删除。", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText("暂无比对记录", { exact: true })).toBeVisible();
  await expect(page.getByRole("heading", { name: "比对完成" })).toHaveCount(0);
  const removals = live(calls, "remove");
  expect(removals).toHaveLength(2);
  expect(removals[1].request.method()).toBe("DELETE");
});

test("the English console translates comparison labels, reasons and counts", async ({
  page,
}) => {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-language", "en-US"),
  );
  await routeComparisons(page, {
    list: () => ({
      items: [
        historyItem(),
        historyItem({
          id: "comparison-3",
          status: "UNAVAILABLE",
          nodeId: "node-c",
          nodeName: "agent-c",
        }),
      ],
      complete: false,
      unavailableNodes: [{ nodeId: "node-c", nodeName: "agent-c", count: 1 }],
    }),
    page: (_, query) => pageOf(progress, rows, query),
  });
  await page.goto("/tools?comparison=comparison-1");
  await expect(
    page.getByRole("heading", { name: "Comparison history" }),
  ).toBeVisible();
  await expect(
    page.getByText(
      "Cannot reach 1 node right now; comparisons there do not show their latest status.",
      { exact: true },
    ),
  ).toBeVisible();
  await expect(page.getByText("Node unavailable").first()).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "Comparison completed" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "All differences 4" }),
  ).toBeVisible();
  await expect(
    page.getByText("Differs: Size, Content-Type, Custom metadata", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(
    page.getByText("Checked 10, 2.5 objects/s, elapsed 4 s", { exact: true }),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "Export ZIP" })).toBeVisible();
  // Longer English labels never make a table scroll sideways.
  expect(
    await page
      .locator(".MuiTableContainer-root")
      .evaluateAll((elements) =>
        elements.filter(
          (element) => element.scrollWidth > element.clientWidth + 1,
        ),
      ),
  ).toHaveLength(0);
});

test("comparison messages have both translations and keep their parameters", () => {
  const parameters = (value: string) =>
    [
      ...new Set(
        [...value.matchAll(/\{([A-Za-z][A-Za-z0-9_]*)\}/g)].map((m) => m[1]),
      ),
    ].sort();
  for (const [source, translations] of Object.entries(comparisonMessages))
    for (const locale of ["en", "ja"] as const) {
      expect(translations[locale].trim(), `${locale}: ${source}`).not.toBe("");
      expect(parameters(translations[locale]), `${locale}: ${source}`).toEqual(
        parameters(source),
      );
    }
});
