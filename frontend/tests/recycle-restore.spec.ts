import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

// The delete confirmation says that tasks move to the recycle bin. The
// recycle bin restores them, as the Master and the CLI always could, instead
// of offering only a permanent deletion.
test("the recycle bin restores selected tasks to the task list", async ({
  page,
}) => {
  await mockApi(page);
  const restored = new Set<number>();
  const submitted: unknown[] = [];
  await page.route("**/api/master/deleted-tasks", (route) =>
    route.fulfill({
      json: {
        success: true,
        deletedTasks: {
          "10.0.1.12": [41, 42, 43]
            .filter((id) => !restored.has(id))
            .map((taskIndex) => ({
              taskIndex,
              sourceBucket: "source",
              destBucket: "dest",
              status: "COMPLETED",
              ...(taskIndex === 43
                ? {
                    cleanup: {
                      status: "RUNNING",
                      phase: "AGENT",
                      recordsDeleted: 10,
                      completedNodes: 0,
                      totalNodes: 1,
                    },
                  }
                : {}),
            })),
        },
        queriedWorkers: ["10.0.1.12"],
        totalCount: 3 - restored.size,
        complete: true,
      },
    }),
  );
  await page.route("**/api/master/restore", (route) => {
    const body = route.request().postDataJSON();
    submitted.push(body);
    body.taskIds.forEach((id: number) => restored.add(id));
    return route.fulfill({
      json: {
        success: true,
        accepted: body.taskIds.length,
        ignored: 0,
        failed: 0,
        details: body.taskIds.map((taskId: number) => ({
          taskId,
          success: true,
          accepted: true,
          status: "COMPLETED",
        })),
      },
    });
  });
  await page.goto("/tools?tab=recycle");
  await expect(
    page.getByText(
      "回收站保留已标记删除的任务，选择后可还原到任务列表。永久删除仅清理任务记录，无法恢复；不删除源端或目标端的数据对象。",
    ),
  ).toBeVisible();
  const restore = page.getByRole("button", { name: "还原", exact: true });
  await expect(restore).toBeDisabled();
  // A task whose permanent deletion is being cleaned up cannot be restored.
  await page
    .getByRole("checkbox", { name: "选择全部已删除任务", exact: true })
    .check();
  await expect(page.getByText("已选择 2 个任务 ID")).toBeVisible();
  await restore.click();
  await expect(
    page.getByText("已还原 2 个任务，可在任务列表中查看。", { exact: true }),
  ).toBeVisible();
  expect(submitted).toEqual([{ taskIds: [42, 41] }]);
  await expect(page.getByText("已选择", { exact: false })).toHaveCount(0);
});
