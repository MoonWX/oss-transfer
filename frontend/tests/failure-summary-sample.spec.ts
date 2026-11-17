import { expect, test } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

// Past 1,000 records the Master classifies a sample of the lowest-numbered
// subtasks. The other causes are then record counts of that sample, not
// "N more" objects next to the exact total.
test("a sampled failure summary shows record counts, not more objects", async ({
  page,
}) => {
  await mockApi(page);
  const failed = {
    ...tasks[0],
    status: "PARTIAL_FAILED",
    failedObjectCount: 7680,
  };
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: {
        success: true,
        totalTasks: tasks.length,
        tasks: tasks.map((task) => (task.taskIndex === 1048 ? failed : task)),
      },
    }),
  );
  const message =
    "7680 个对象失败。按编号最小的子任务中的 1000 条失败记录统计，最多的原因（640 条）：连接存储超时";
  await page.route("**/api/master/tasks/1048", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          ...failed,
          failureSummary: {
            failed: 7680,
            allFailed: false,
            sampled: 1000,
            truncated: true,
            causes: [
              { code: "TIMEOUT", count: 640, message: "连接存储超时" },
              {
                code: "ACCESS_DENIED",
                count: 360,
                message: "存储拒绝访问，请检查凭据和存储桶权限",
              },
            ],
            message,
          },
          attention: { code: "OBJECTS_FAILED", severity: "error", message },
        },
      },
    }),
  );
  await page.goto("/tasks?taskId=1048");
  const attention = page.getByTestId("task-attention");
  await expect(attention).toContainText(message);
  await expect(attention).toContainText(
    "360 条：存储拒绝访问，请检查凭据和存储桶权限",
  );
  await expect(attention).not.toContainText("另有");
});
