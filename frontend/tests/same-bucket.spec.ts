import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

for (const kind of ["csv", "buckets", "tasks"] as const) {
  test(`${kind} import rejects identical OSS endpoints before submitting`, async ({
    page,
  }) => {
    const requests = await mockApi(page);
    await page.goto("/create");
    await page.getByRole("tab", { name: "批量导入" }).click();
    if (kind === "csv") {
      await page.getByRole("combobox", { name: "数据格式" }).click();
      await page.getByRole("option", { name: "逗号分隔文本" }).click();
    } else if (kind === "tasks") {
      await page.getByRole("combobox", { name: "导入类型" }).click();
      await page.getByRole("option", { name: "OSS 子任务恢复 / 导入" }).click();
    }
    const valid = {
      sourceS3Alias: "production",
      sourceBucket: "source",
      destS3Alias: "production",
      destBucket: "destination",
    };
    await page.getByLabel("任务数据").fill(
      kind === "csv"
        ? "production/source,production/destination\nproduction/same,production/same,,true"
        : JSON.stringify([
            valid,
            {
              ...valid,
              sourceBucket: " same ",
              destBucket: "same",
              overwrite: true,
            },
          ]),
    );
    await page.getByRole("button", { name: "导入任务", exact: true }).click();
    await expect(
      page.getByText(
        "第 2 项：源和目标不能是同一存储桶，请选择其他目标存储桶。",
      ),
    ).toBeVisible();
    expect(
      requests.filter(
        ({ path }) =>
          path === "/api/master/add-buckets" || path === "/api/master/import",
      ),
    ).toHaveLength(0);
  });
}

test("distinct OSS endpoints still submit and retain partial acceptance feedback", async ({
  page,
}) => {
  await mockApi(page);
  const submissions: unknown[] = [];
  await page.route("**/api/master/add-buckets", async (route) => {
    submissions.push(route.request().postDataJSON());
    await route.fulfill({ json: { success: true, addedCount: 1 } });
  });
  await page.goto("/create");
  await page.getByRole("tab", { name: "批量导入" }).click();
  const rows = [
    {
      sourceS3Alias: "production",
      sourceBucket: "source",
      destS3Alias: "production",
      destBucket: "destination",
    },
    {
      sourceS3Alias: "production",
      sourceBucket: "same",
      destS3Alias: "backup",
      destBucket: "same",
    },
  ];
  await page.getByLabel("任务数据").fill(JSON.stringify(rows));
  await page.getByRole("button", { name: "导入任务", exact: true }).click();
  await expect(page.getByText("提交结果待核对", { exact: true })).toBeVisible();
  await expect(page.getByText(/提交 2 个任务，已受理 1 个/)).toBeVisible();
  await expect(page.getByText(/避免重复提交/)).toBeVisible();
  expect(submissions).toEqual([rows]);
});
