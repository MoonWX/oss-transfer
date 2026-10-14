import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

test("lost creation response retains identity across reload; a deliberate repeat gets a new identity", async ({
  page,
}) => {
  await mockApi(page);
  const accepted = new Map<string, number>();
  const requests: string[] = [];
  await page.route("**/api/master/add-buckets", async (route) => {
    const id = route.request().headers()["idempotency-key"];
    expect(id).toMatch(/^[a-f0-9]{32}$/);
    requests.push(id);
    if (!accepted.has(id)) accepted.set(id, 1050 + accepted.size);
    if (requests.length === 1) {
      // The server has accepted the request, but the browser never receives
      // its response. Reloading must not turn a retry into a second task.
      await route.abort("failed");
      return;
    }
    await route.fulfill({
      json: {
        success: true,
        addedCount: 1,
        taskIds: [accepted.get(id)],
      },
    });
  });
  await page.goto("/create");
  await page.getByRole("combobox", { name: "源存储连接" }).click();
  await page.getByRole("option", { name: "production", exact: true }).click();
  await page.getByRole("combobox", { name: "源存储桶" }).fill("media-archive");
  await page.getByRole("combobox", { name: "目标存储连接" }).click();
  await page.getByRole("option", { name: "backup", exact: true }).click();
  await page.getByRole("combobox", { name: "目标存储桶" }).fill("media-backup");
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
  await expect(page.getByText("任务提交失败", { exact: true })).toBeVisible();
  expect(accepted.size).toBe(1);

  await page.reload();
  await expect(page.getByRole("combobox", { name: "源存储桶" })).toHaveValue(
    "media-archive",
  );
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "继续配置任务" }),
  ).toBeVisible();
  await expect(
    page.getByText("迁移任务已提交", { exact: true }).first(),
  ).toBeVisible();
  expect(requests).toEqual([requests[0], requests[0]]);
  expect(accepted.size).toBe(1);
  await expect
    .poll(() =>
      page.evaluate(() => sessionStorage.getItem("oss-transfer-create-draft")),
    )
    .toBeNull();

  await page.getByRole("button", { name: "继续配置任务" }).click();
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "继续配置任务" }),
  ).toBeVisible();
  expect(requests).toHaveLength(3);
  expect(requests[2]).not.toBe(requests[0]);
  expect(accepted.size).toBe(2);
});

test("partial acceptance keeps the creation identity when the page is reloaded", async ({
  page,
}) => {
  await mockApi(page);
  const identities: string[] = [];
  await page.route("**/api/master/add-buckets", async (route) => {
    identities.push(route.request().headers()["idempotency-key"]);
    await route.fulfill({
      json: {
        success: true,
        addedCount: identities.length === 1 ? 1 : 2,
        taskIds: [1050],
        partial: identities.length === 1,
      },
    });
  });
  await page.goto("/create");
  await page.getByRole("tab", { name: "批量导入" }).click();
  await page.getByLabel("任务数据").fill(
    JSON.stringify([
      { sourceBucket: "bucket-a", destBucket: "bucket-b" },
      { sourceBucket: "bucket-c", destBucket: "bucket-d" },
    ]),
  );
  await page.getByRole("button", { name: "导入任务", exact: true }).click();
  await expect(page.getByText("提交结果待核对", { exact: true })).toBeVisible();
  await page.reload();
  await page.getByRole("button", { name: "导入任务", exact: true }).click();
  await expect(
    page.getByText("迁移任务已提交", { exact: true }).first(),
  ).toBeVisible();
  expect(identities).toHaveLength(2);
  expect(identities[0]).toMatch(/^[a-f0-9]{32}$/);
  expect(identities[1]).toBe(identities[0]);
});
