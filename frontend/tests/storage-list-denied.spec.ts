import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

// Keys limited to one bucket cannot list buckets. The connection test says
// that the listing was refused, instead of a green "Found 0 buckets" that
// reads like an empty or wrong account.
test("a refused bucket listing is not reported as zero buckets found", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/s3-config/production/buckets**", (route) =>
    route.fulfill({
      json: {
        success: true,
        buckets: [],
        pathStyle: false,
        pathStyleAuto: true,
        listBucketsDenied: true,
        warning: "该凭据无权列举存储桶（ListBuckets 被拒绝）",
      },
    }),
  );
  await page.goto("/storage");
  await page
    .getByRole("button", { name: "测试连接", exact: true })
    .first()
    .click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByTestId("storage-list-denied")).toHaveText(
    "该凭据无权列举存储桶，未获取存储桶列表。",
  );
  await expect(dialog.getByText(/已获取 \d+ 个存储桶/)).toHaveCount(0);
  await expect(dialog.getByTestId("storage-check-warning")).toHaveText(
    "该凭据无权列举存储桶（ListBuckets 被拒绝）",
  );
});
