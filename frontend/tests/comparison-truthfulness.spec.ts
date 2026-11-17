import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";
import {
  historyItem,
  pageOf,
  progress,
  routeComparisons,
  row,
  rows,
} from "./object-comparison-fixtures";

// An Agent before results protocol 2 reports a fixed row limit. Its counts
// are known to be unreliable, and the result says so.
test("a result from an older execution node says that its counts may be wrong", async ({
  page,
}) => {
  await mockApi(page);
  const legacy = {
    ...progress,
    compared: 1000000,
    matched: 12,
    different: 0,
    etagMismatch: 0,
    missing: 0,
    extra: 0,
    changed: 999988,
    rows: 4,
    resultLimit: 10000,
    resultsProtocol: undefined,
  };
  let current = legacy;
  await routeComparisons(page, {
    list: () => ({ items: [historyItem(current)], complete: true }),
    page: (_, query) => pageOf(current, rows, query),
  });
  await page.goto("/tools?comparison=comparison-1");
  const note = page.getByTestId("legacy-comparison-node");
  await expect(note).toHaveText(
    "该结果来自 4.1.40 及更早版本的执行节点，统计可能不准确：在 LIST 时间精确到毫秒的存储上，“比对期间已变化”可能是误报，也可能包含真正缺失的对象；单个对象的比对中，“两端均不存在”计为“读取失败”；最多保存 1 万条差异。请升级该节点后重新比对。",
  );
  current = progress;
  await page.reload();
  await expect(page.getByText("一致 6", { exact: true })).toBeVisible();
  await expect(note).toHaveCount(0);
});

// A side that was not read, or that does not exist, has no size. It shows
// "—", not 0 B, which is the size of an empty object.
test("a side whose size was not read shows no size", async ({ page }) => {
  await mockApi(page);
  const result = {
    ...progress,
    compared: 9,
    matched: 6,
    different: 1,
    etagMismatch: 0,
    missing: 0,
    extra: 0,
    changed: 1,
    errors: 1,
    rows: 3,
  };
  const stored = [
    {
      ...row(1, "ERROR", {
        sourceSize: 2048,
        destinationSize: 0,
        error: "无法读取目标元数据",
        errorDetail: "target HEAD: HTTP 403 Forbidden",
      }),
      destinationSizeUnknown: true,
    },
    {
      ...row(2, "CHANGED_DURING_COMPARE", {
        sourceSize: 0,
        destinationSize: 1024,
      }),
      sourceSizeUnknown: true,
    },
    row(3, "DIFFERENT", {
      sourceSize: 0,
      destinationSize: 0,
      reasons: ["contentType"],
    }),
  ];
  await routeComparisons(page, {
    list: () => ({ items: [historyItem(result)], complete: true }),
    page: (_, query) => pageOf(result, stored, query),
  });
  await page.goto("/tools?comparison=comparison-1");
  const line = (number: number) =>
    page.getByRole("row").filter({ hasText: `2026/file-000${number}.bin` });
  await expect(line(1)).toContainText("2 KiB");
  await expect(line(1)).toContainText("—");
  await expect(line(1)).not.toContainText("0 B");
  await expect(line(2)).toContainText("1 KiB");
  await expect(line(2)).toContainText("—");
  await expect(line(2)).not.toContainText("0 B");
  // An empty object keeps its real size.
  await expect(line(3).getByText("0 B", { exact: true })).toHaveCount(2);
});
