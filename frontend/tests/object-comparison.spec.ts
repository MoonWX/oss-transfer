import { test, expect } from "@playwright/test";
import { mockApi } from "./fixtures";

const progress = {
  id: "comparison-1",
  status: "COMPLETED",
  mode: "metadata",
  scope: "prefix",
  source: { ref: "source", bucket: "photos" },
  destination: { ref: "target", bucket: "archive" },
  sourcePrefix: "2026/",
  destinationPrefix: "backup/",
  compared: 10,
  matched: 8,
  different: 1,
  missing: 1,
  extra: 0,
  changed: 0,
  errors: 0,
  rows: 2,
  items: [
    {
      number: 1,
      key: "a.bin",
      sourceKey: "2026/a.bin",
      destinationKey: "backup/a.bin",
      status: "DIFFERENT",
      sourceSize: 4,
      destinationSize: 8,
    },
    {
      number: 2,
      key: "b.bin",
      sourceKey: "2026/b.bin",
      destinationKey: "backup/b.bin",
      status: "MISSING_TARGET",
      sourceSize: 4,
      destinationSize: 0,
    },
  ],
};

test("range comparison uses exact prefixes, explains metadata limits and restores by URL", async ({
  page,
}) => {
  await mockApi(page);
  let body: unknown;
  await page.route("**/api/master/comparisons", (route) => {
    body = route.request().postDataJSON();
    return route.fulfill({
      status: 202,
      json: { ...progress, status: "RUNNING" },
    });
  });
  await page.route("**/api/master/comparisons/comparison-1?**", (route) =>
    route.fulfill({ json: progress }),
  );
  await page.goto("/tools");
  await page.getByRole("textbox", { name: "源 Bucket" }).fill("source/photos");
  await page
    .getByRole("textbox", { name: "目标 Bucket" })
    .fill("target/archive");
  await page.getByRole("textbox", { name: "源前缀" }).fill("2026/ + ");
  await page.getByRole("textbox", { name: "目标前缀" }).fill("backup/");
  await expect(page.getByRole("alert")).toContainText("不读取对象内容");
  await expect(page.getByRole("alert")).toContainText(
    "ETag、修改时间和存储类型不作为一致依据",
  );
  await page.getByRole("button", { name: "开始比对" }).click();
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
  await page.reload();
  await expect(
    page.getByRole("cell", { name: "2026/a.bin", exact: true }),
  ).toBeVisible();
});

test("content mode submits streaming SHA256 and cancellation keeps current results", async ({
  page,
}) => {
  await mockApi(page);
  let state = "RUNNING";
  let submitted: Record<string, string> = {};
  let canceled = false;
  await page.route("**/api/master/comparisons", (route) => {
    submitted = route.request().postDataJSON();
    return route.fulfill({
      status: 202,
      json: { ...progress, mode: "content", status: state },
    });
  });
  await page.route("**/api/master/comparisons/comparison-1?**", (route) =>
    route.fulfill({ json: { ...progress, mode: "content", status: state } }),
  );
  await page.route("**/api/master/comparisons/comparison-1/cancel", (route) => {
    canceled = true;
    state = "CANCELED";
    return route.fulfill({
      json: { ...progress, mode: "content", status: state },
    });
  });
  await page.goto(
    "/tools?sourceBucket=source/photos&destBucket=target/archive&sourceKey=a.bin&destKey=b.bin",
  );
  await page.getByRole("combobox", { name: "比对方式" }).click();
  await page.getByRole("option", { name: "内容 SHA-256（流式）" }).click();
  await expect(page.getByRole("alert")).toContainText(
    "不保存对象文件到本地磁盘",
  );
  await page.getByRole("button", { name: "开始比对" }).click();
  await expect(page.getByRole("button", { name: "取消比对" })).toBeVisible();
  expect(submitted.mode).toBe("content");
  expect(submitted.scope).toBe("object");
  expect(submitted.sourcePrefix).toBe("a.bin");
  await page.getByRole("button", { name: "取消比对" }).click();
  await expect(page.getByRole("heading", { name: "已取消" })).toBeVisible();
  expect(canceled).toBe(true);
  await expect(
    page.getByRole("cell", { name: "2026/a.bin", exact: true }),
  ).toBeVisible();
});

test("difference pagination appends and does not reset on progress polling", async ({
  page,
}) => {
  await mockApi(page);
  let reads = 0;
  await page.route("**/api/master/comparisons/comparison-1?**", (route) => {
    const after = new URL(route.request().url()).searchParams.get("after");
    reads++;
    return route.fulfill({
      json: {
        ...progress,
        status: "RUNNING",
        items: after ? progress.items.slice(1) : progress.items.slice(0, 1),
        nextAfter: after ? 0 : 1,
      },
    });
  });
  await page.goto("/tools?comparison=comparison-1");
  await page.getByRole("button", { name: "加载更多差异" }).click();
  await expect(
    page.getByRole("cell", { name: "2026/b.bin", exact: true }),
  ).toBeVisible();
  await expect.poll(() => reads).toBeGreaterThan(2);
  await expect(
    page.getByRole("cell", { name: "2026/b.bin", exact: true }),
  ).toBeVisible();
});

test("read failures and concurrent changes never appear as confirmed missing or matching", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/comparisons/comparison-1?**", (route) =>
    route.fulfill({
      json: {
        ...progress,
        compared: 2,
        matched: 0,
        different: 0,
        missing: 0,
        errors: 1,
        changed: 1,
        items: [
          {
            ...progress.items[0],
            status: "ERROR",
            error: "无法读取目标元数据",
          },
          { ...progress.items[1], status: "CHANGED_DURING_COMPARE" },
        ],
      },
    }),
  );
  await page.goto("/tools?comparison=comparison-1");
  await expect(page.getByRole("cell", { name: /读取失败/ })).toBeVisible();
  await expect(
    page.getByRole("cell", { name: "比对期间已变化", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("cell", { name: "目标缺失", exact: true }),
  ).toHaveCount(0);
  await expect(page.getByText("一致 0", { exact: true })).toBeVisible();
});
