import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";
import {
  historyItem,
  pageOf,
  progress,
  routeComparisons,
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
