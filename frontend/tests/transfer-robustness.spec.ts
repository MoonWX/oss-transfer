import { expect, test } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

test("exceptions explain sources that changed or were deleted after the scan", async ({
  page,
}) => {
  await mockApi(page);
  const time = Date.parse("2026-11-10T06:00:00Z");
  const row = (key: string, extra: object) => ({
    parentTaskId: 1,
    taskIndex: 1,
    childTaskId: "1-1",
    key,
    errorCode: "TRANSFER_FAILED",
    reasonCode: "SOURCE_CHANGED",
    errorMessage: `open source "${key}": source changed`,
    timestamp: time,
    retryCount: 1,
    retryUpdatedAt: time + 60000,
    direction: "OSS_TO_LOCAL",
    ...extra,
  });
  const rows = [
    row("medium/m7.bin", { retryStatus: "NOT_RETRIED", retryCount: 0 }),
    row("medium/m8.bin", {
      retryStatus: "SKIPPED",
      retryReasonCode: "SOURCE_DELETED",
    }),
  ];
  await page.route("**/api/master/tasks/1", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: { ...tasks[2], taskIndex: 1, status: "PARTIAL_FAILED" },
      },
    }),
  );
  await page.route("**/api/master/local-transfers/failures?**", (route) =>
    route.fulfill({
      json: { success: true, data: { items: rows, total: rows.length } },
    }),
  );
  await page.goto("/exceptions?type=local&taskId=1");
  const region = page.getByRole("region", {
    name: "本地迁移失败",
    includeHidden: true,
  });
  const changed = region
    .getByRole("row", { includeHidden: true })
    .filter({ hasText: "medium/m7.bin" });
  const deleted = region
    .getByRole("row", { includeHidden: true })
    .filter({ hasText: "medium/m8.bin" });
  await expect(changed.getByTestId("exception-reason")).toHaveText(
    "源对象在扫描后已变化，重试会读取其当前版本",
  );
  await expect(changed.getByTestId("exception-retry-reason")).toHaveCount(0);
  await expect(deleted.getByTestId("exception-retry-reason")).toHaveText(
    "源对象已在源端删除，已跳过",
  );
  await deleted.getByRole("button", { name: "详情", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "异常详情" });
  await expect(dialog).toContainText(
    "源对象已在源端删除，已跳过 (SOURCE_DELETED)",
  );
  await expect(dialog).toContainText("重试原因");
});

test("the task detail names the main failure cause", async ({ page }) => {
  await mockApi(page);
  const failed = {
    ...tasks[0],
    status: "FAILED",
    failedObjectCount: 18,
    putObjectCount: 0,
    skippedObjectCount: 0,
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
  await page.route("**/api/master/tasks/1048", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          ...failed,
          failureSummary: {
            failed: 18,
            allFailed: true,
            causes: [
              {
                code: "LOCAL_PERMISSION_DENIED",
                count: 17,
                message: "执行节点没有本地目录的读写权限",
              },
              { code: "NO_SPACE", count: 1, message: "目标磁盘空间不足" },
            ],
          },
          attention: {
            code: "OBJECTS_FAILED",
            severity: "error",
            message: "全部 18 个对象失败：执行节点没有本地目录的读写权限",
          },
        },
      },
    }),
  );
  await page.goto("/tasks?taskId=1048");
  const attention = page.getByTestId("task-attention");
  await expect(attention).toContainText(
    "全部 18 个对象失败：执行节点没有本地目录的读写权限",
  );
  await expect(attention).toContainText("另有 1 个：目标磁盘空间不足");
  await expect(attention).toContainText("可在“异常记录”中查看失败对象并重试。");
});

test("the task detail says when every node of a running task is offline", async ({
  page,
}) => {
  await mockApi(page);
  const lastSeen = Date.parse("2026-11-10T00:16:38Z");
  await page.route("**/api/master/tasks/1048", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          ...tasks[0],
          offlineNodes: [
            {
              executionNodeIp: "10.0.1.12",
              displayLabel: "ot-eval-fail-agent1 (10.0.1.12)",
              lastSeen,
            },
          ],
          attention: {
            code: "NODES_OFFLINE",
            severity: "warning",
            message:
              "执行节点 ot-eval-fail-agent1 (10.0.1.12) 离线，任务等待其恢复连接",
          },
        },
      },
    }),
  );
  await page.goto("/tasks?taskId=1048");
  const attention = page.getByTestId("task-attention");
  await expect(attention).toContainText(
    "执行节点 ot-eval-fail-agent1 (10.0.1.12) 离线，任务等待其恢复连接",
  );
  await expect(attention).toContainText(
    "ot-eval-fail-agent1 (10.0.1.12) 最后连接于",
  );
});

test("a download precheck shows the free space of a writable destination", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/local-transfers/validate", (route) => {
    const body = route.request().postDataJSON();
    return route.fulfill({
      json: {
        success: true,
        data: {
          valid: true,
          nodeBindings: body[0].destinationEndpoint.nodeBindings.map(
            (node: object) => ({
              ...node,
              valid: true,
              pathType: "DIRECTORY",
              freeBytes: 13207024435,
              message: "目标目录可写，可用空间 12.3 GiB",
            }),
          ),
        },
      },
    });
  });
  await page.goto("/create");
  await page.getByRole("button", { name: /对象存储 → 本地/ }).click();
  await page.getByRole("combobox", { name: "源存储连接", exact: true }).click();
  await page.getByRole("option", { name: "production", exact: true }).click();
  await page
    .getByRole("combobox", { name: "源存储桶", exact: true })
    .fill("media-archive");
  await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
  await page.getByRole("option", { name: /10\.0\.1\.12/ }).click();
  await page.getByLabel("目标目录绝对路径").fill("/mnt/data");
  await page.getByRole("button", { name: "运行预检", exact: true }).click();
  await expect(page.getByText("节点预检通过", { exact: true })).toBeVisible();
  await expect(
    page.getByText("目标目录可写，可用空间 12.3 GiB", { exact: true }),
  ).toBeVisible();
});
