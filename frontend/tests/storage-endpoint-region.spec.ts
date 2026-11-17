import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

const ossReply =
  "operation error S3: ListObjectsV2, https response error StatusCode: 403, RequestID: 5F1A, HostID: a-logs.oss-cn-hangzhou.aliyuncs.com, api error AccessDenied: The bucket you are attempting to access must be addressed using the specified endpoint. Please send all future requests to this endpoint. [storage response: http=403, server=AliyunOSS, body=xml, endpoint=oss-cn-beijing.aliyuncs.com]";

// The connection test shows the provider's own reply under the warning: it
// names the endpoint the bucket needs.
test("the connection test can show the raw reply under its warning", async ({
  page,
}) => {
  await mockApi(page);
  const warning =
    "连接可用，但对第一个列出的存储桶 a-logs 的检查失败：存储桶 a-logs 需要通过 Endpoint oss-cn-beijing.aliyuncs.com 访问，这通常是该存储桶所在地域的 Endpoint。如需迁移此存储桶，请使用该 Endpoint 创建存储连接。";
  await page.route("**/api/master/s3-config/production/buckets**", (route) =>
    route.fulfill({
      json: {
        success: true,
        buckets: ["a-logs", "b-data"],
        pathStyle: false,
        pathStyleAuto: true,
        bucketCheckFailed: "a-logs",
        warning,
        warningDetail: ossReply,
        warningCode: "STORAGE_WRONG_ENDPOINT",
      },
    }),
  );
  await page.goto("/storage");
  await page
    .getByRole("button", { name: "测试连接", exact: true })
    .first()
    .click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByTestId("storage-check-warning")).toHaveText(warning);
  const detail = dialog.getByTestId("storage-warning-detail");
  await expect(detail).toBeHidden();
  await dialog.getByRole("button", { name: "显示原始错误" }).click();
  await expect(detail).toHaveText(ossReply);
});

// A failed object on such a bucket names the endpoint, not the permissions.
test("an exception on a bucket of another region names the endpoint it needs", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/failed", (route) =>
    route.fulfill({
      json: {
        success: true,
        failedKeys: {
          "10.0.1.12": [
            {
              parentTaskId: 9,
              taskIndex: 9,
              childTaskId: "9-1",
              key: "logs/2026.gz",
              errorMessage: `open source "logs/2026.gz": ${ossReply}`,
              retryStatus: "NOT_RETRIED",
              workerNode: "10.0.1.12",
              direction: "OSS_TO_OSS",
            },
          ],
          "10.0.1.12Count": 1,
        },
        queriedWorkers: ["10.0.1.12"],
        complete: true,
        totalCount: 1,
      },
    }),
  );
  await page.goto("/exceptions");
  const table = page.getByRole("table");
  await expect(table).toContainText(
    "存储桶需要通过 Endpoint oss-cn-beijing.aliyuncs.com 访问，这通常是该存储桶所在地域的 Endpoint。",
  );
  await expect(table).not.toContainText("请检查存储桶权限");
});
