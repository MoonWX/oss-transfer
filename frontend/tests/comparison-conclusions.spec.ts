import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";
import {
  historyItem,
  pageOf,
  progress,
  routeComparisons,
} from "./object-comparison-fixtures";

const nothing = {
  ...progress,
  compared: 0,
  matched: 0,
  different: 0,
  etagMismatch: 0,
  missing: 0,
  extra: 0,
  rows: 0,
};

// A completed comparison that checked no object found nothing to compare;
// that is not a finding that the objects match.
test("a comparison that checked nothing warns instead of finding no differences", async ({
  page,
}) => {
  await mockApi(page);
  await routeComparisons(page, {
    list: () => ({ items: [historyItem(nothing)], complete: true }),
    page: (_, query) => pageOf(nothing, [], query),
  });
  await page.goto("/tools?comparison=comparison-1");
  await expect(page.getByTestId("comparison-empty")).toHaveText(
    "没有找到任何对象。请检查两端的存储桶和前缀：Key 区分大小写，通常不以 / 开头。",
  );
  await expect(page.getByText("未发现差异。", { exact: true })).toHaveCount(0);
  const history = page.getByRole("table", { name: "比对历史" });
  await expect(history.getByText("未找到对象", { exact: true })).toBeVisible();
  await expect(history.getByText("比对完成", { exact: true })).toHaveCount(0);
});

// A comparison that did not finish has not established anything about the
// objects it did not reach.
test("an interrupted comparison without differences says it did not finish", async ({
  page,
}) => {
  await mockApi(page);
  let current: Record<string, unknown> = {
    ...nothing,
    status: "INTERRUPTED",
    compared: 120,
    matched: 120,
    resumable: true,
  };
  await routeComparisons(page, {
    list: () => ({ items: [historyItem(current)], complete: true }),
    page: (_, query) => pageOf(current, [], query),
  });
  await page.goto("/tools?comparison=comparison-1");
  await expect(
    page.getByText("比对未完成：已检查的 120 个对象中暂未发现差异。", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(page.getByText("未发现差异。", { exact: true })).toHaveCount(0);
  current = { ...current, compared: 0, matched: 0 };
  await page.getByRole("button", { name: "刷新" }).click();
  await expect(
    page.getByText("比对未完成，尚未检查任何对象。", { exact: true }),
  ).toBeVisible();
});

test("the comparison form points out prefixes that rarely match a key", async ({
  page,
}) => {
  await mockApi(page);
  await routeComparisons(page, { list: () => ({ items: [], complete: true }) });
  await page.goto("/tools");
  const source = page.getByRole("textbox", { name: "源前缀", exact: true });
  await source.fill("/2026/");
  await expect(
    page.getByText("以 / 开头。对象 Key 通常不以 / 开头。", { exact: true }),
  ).toBeVisible();
  const target = page.getByRole("textbox", { name: "目标前缀", exact: true });
  await target.fill("backup/ ");
  await expect(
    page.getByText("首尾有空白字符。Key 按原样匹配，空白也会参与匹配。", {
      exact: true,
    }),
  ).toBeVisible();
  await source.fill("2026/");
  await target.fill("backup/");
  await expect(page.getByText(/以 \/ 开头|首尾有空白字符/)).toHaveCount(0);
});
