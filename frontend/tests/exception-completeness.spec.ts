import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

// The Master never asks Agents for failures: it reads its own index and
// names only the offline nodes that still own unfinished subtasks. The page
// must say that, not that nodes "did not return data".
test("an incomplete failure list names offline nodes with unfinished subtasks", async ({
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
              parentTaskId: 7,
              taskIndex: 7,
              childTaskId: "7-1",
              key: "failed.bin",
              errorMessage: "Upload failed",
              retryStatus: "NOT_RETRIED",
              workerNode: "10.0.1.12",
              direction: "OSS_TO_OSS",
            },
          ],
          "10.0.1.12Count": 1,
          "10.0.1.13": [],
          "10.0.1.13Count": 0,
        },
        totalCount: 1,
        queriedWorkers: ["10.0.1.12", "10.0.1.13"],
        failedWorkers: ["10.0.1.13"],
        unreachableWorkers: ["10.0.1.13"],
        complete: false,
        partial: true,
        truncated: false,
      },
    }),
  );
  await page.goto("/exceptions");
  const banner = page
    .getByRole("alert")
    .filter({ hasText: "未完成子任务的失败记录可能尚未上报" });
  await expect(banner).toBeVisible();
  await expect(banner).toContainText("当前结果可能不完整");
  await expect(page.getByText(/部分节点未返回数据/)).toHaveCount(0);
  await expect(page.getByText("failed.bin")).toBeVisible();
});
