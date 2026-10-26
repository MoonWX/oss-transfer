import { expect, test } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

function gate() {
  let release!: () => void;
  const promise = new Promise<void>((resolve) => {
    release = resolve;
  });
  return { promise, release };
}

test("object retry results refresh without losing original failures or replaying the request", async ({
  page,
}) => {
  await mockApi(page);
  const originalTime = Date.parse("2026-10-26T06:00:00Z");
  const resultTime = originalTime + 60000;
  let parent = {
    ...tasks[2],
    taskIndex: 1,
    generation: 1,
    status: "PARTIAL_FAILED",
  };
  let rows = ["recovered.bin", "failed-again.bin", "skipped.bin"].map(
    (key) => ({
      parentTaskId: 1,
      taskIndex: 1,
      childTaskId: "1-1",
      key,
      errorMessage: "Original upload failure",
      timestamp: originalTime,
      retryStatus: "NOT_RETRIED",
      retryCount: 0,
      retryUpdatedAt: 0,
      retryError: "",
      direction: "LOCAL_TO_OSS",
    }),
  );
  let sent = 0,
    reads = 0;
  const pendingPoll = gate();
  await page.route("**/api/master/tasks/1", (route) =>
    route.fulfill({ json: { success: true, data: parent } }),
  );
  await page.route(
    "**/api/master/local-transfers/failures?**",
    async (route) => {
      reads++;
      if (reads === 3) await pendingPoll.promise;
      await route.fulfill({
        json: { success: true, data: { items: rows, total: rows.length } },
      });
    },
  );
  await page.route("**/api/master/retry", (route) => {
    sent++;
    expect(route.request().postDataJSON()).toEqual({
      taskIds: [1],
      expectedGenerations: { "1": 1 },
    });
    parent = { ...parent, status: "RUNNING", generation: 2 };
    rows = rows.map((row) => ({
      ...row,
      retryStatus: "PENDING",
      retryCount: 1,
    }));
    return route.fulfill({
      json: {
        success: true,
        accepted: 1,
        ignored: 0,
        failed: 0,
        details: [
          {
            taskId: 1,
            status: "RUNNING",
            generation: 2,
            accepted: true,
            success: true,
          },
        ],
      },
    });
  });
  try {
    await page.goto("/exceptions?type=local&taskId=1");
    const region = page.getByRole("region", {
      name: "本地迁移失败",
      includeHidden: true,
    });
    const recovered = region
      .getByRole("row", { includeHidden: true })
      .filter({ hasText: "recovered.bin" });
    const failed = region
      .getByRole("row", { includeHidden: true })
      .filter({ hasText: "failed-again.bin" });
    const skipped = region
      .getByRole("row", { includeHidden: true })
      .filter({ hasText: "skipped.bin" });
    await expect(region.getByText("未重试", { exact: true })).toHaveCount(3);
    const actions = page.getByRole("region", { name: "任务操作" });
    await expect(actions.getByText("任务 #1", { exact: true })).toBeVisible();
    await actions
      .getByRole("button", { name: "重试失败项", exact: true })
      .dblclick();
    await expect(region.getByText("重试中", { exact: true })).toHaveCount(3);
    await recovered.getByRole("button", { name: "详情", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "异常详情" });
    await expect(dialog.getByText("重试中", { exact: true })).toBeVisible();
    await expect.poll(() => reads).toBeGreaterThanOrEqual(3);
    // A slow background refresh retains the rows and does not flash a loader.
    await expect(region).toHaveAttribute("aria-busy", "false");
    await expect(recovered).toContainText("Original upload failure");
    await expect(
      page.getByRole("progressbar", { name: "正在读取记录" }),
    ).toHaveCount(0);
    rows = rows.map((row, index) => ({
      ...row,
      retryStatus: ["RECOVERED", "FAILED", "SKIPPED"][index],
      retryUpdatedAt: resultTime,
      retryError: index === 1 ? "Retry: permission denied" : "",
    }));
    parent = { ...parent, status: "PARTIAL_FAILED" };
    pendingPoll.release();
    await expect(dialog.getByText("已恢复", { exact: true })).toBeVisible();
    await expect(
      dialog.getByText("Original upload failure", { exact: true }),
    ).toBeVisible();
    await dialog.getByRole("button", { name: "关闭", exact: true }).click();
    await expect(recovered.getByText("已恢复", { exact: true })).toBeVisible();
    await expect(skipped.getByText("已跳过", { exact: true })).toBeVisible();
    await expect(failed.getByText("再次失败", { exact: true })).toBeVisible();
    await expect(failed).toContainText("Original upload failure");
    await expect(failed).toContainText("Retry: permission denied");
    await expect(failed).toContainText("重试 1 次");
    await expect(
      recovered.getByRole("button", { name: "重试任务", exact: true }),
    ).toHaveCount(0);
    await expect(
      skipped.getByRole("button", { name: "重试任务", exact: true }),
    ).toHaveCount(0);
    await expect(
      actions.getByRole("button", { name: "重试失败项", exact: true }),
    ).toBeEnabled({ timeout: 10000 });
    await expect(failed.getByRole("button", { name: /重试/ })).toHaveCount(0);
    expect(sent).toBe(1);
    await failed.getByRole("button", { name: "详情", exact: true }).click();
    await expect(
      dialog.getByText("Retry: permission denied", { exact: true }),
    ).toBeVisible();
    await expect(
      dialog.getByText("重试结果时间", { exact: true }),
    ).toBeVisible();
  } finally {
    pendingPoll.release();
  }
});

test("a completed parent does not invent a recovered object result", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/tasks/1", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: { ...tasks[2], taskIndex: 1, status: "COMPLETED", generation: 2 },
      },
    }),
  );
  await page.route("**/api/master/local-transfers/failures?**", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          items: [
            {
              parentTaskId: 1,
              key: "unconfirmed.bin",
              errorMessage: "Original failure",
              retryStatus: "NOT_RETRIED",
              retryCount: 0,
            },
          ],
          total: 1,
        },
      },
    }),
  );
  await page.goto("/exceptions?type=local&taskId=1");
  const row = page
    .getByRole("row", { includeHidden: true })
    .filter({ hasText: "unconfirmed.bin" });
  await expect(row.getByText("未重试", { exact: true })).toBeVisible();
  await expect(row.getByText("已恢复", { exact: true })).toHaveCount(0);
  await expect(
    page
      .getByRole("region", { name: "任务操作" })
      .getByRole("button", { name: "任务已完成", exact: true }),
  ).toBeDisabled();
});

for (const retryStatus of ["INTERRUPTED", "PENDING"]) {
  test(`a stopped task can resume an unfinished ${retryStatus} object retry`, async ({
    page,
  }) => {
    await mockApi(page);
    let sent = 0;
    let parent = {
      ...tasks[2],
      taskIndex: 1,
      status: "STOPPED",
      generation: 2,
    };
    await page.route("**/api/master/tasks/1", (route) =>
      route.fulfill({ json: { success: true, data: parent } }),
    );
    await page.route("**/api/master/local-transfers/failures?**", (route) =>
      route.fulfill({
        json: {
          success: true,
          data: {
            items: [
              {
                parentTaskId: 1,
                key: "unfinished.bin",
                errorMessage: "Original failure",
                retryStatus,
                retryCount: 1,
              },
            ],
            total: 1,
          },
        },
      }),
    );
    await page.route("**/api/master/resume", (route) => {
      sent++;
      expect(route.request().postDataJSON()).toEqual({
        taskIds: [1],
        expectedGenerations: { "1": 2 },
      });
      parent = { ...parent, status: "RUNNING", generation: 3 };
      return route.fulfill({
        json: {
          success: true,
          accepted: 1,
          ignored: 0,
          failed: 0,
          details: [
            { taskId: 1, generation: 3, status: "RUNNING", accepted: true },
          ],
        },
      });
    });
    await page.goto("/exceptions?type=local&taskId=1");
    const row = page
      .getByRole("row", { includeHidden: true })
      .filter({ hasText: "unfinished.bin" });
    const actions = page.getByRole("region", { name: "任务操作" });
    await expect(
      actions.getByRole("button", { name: "继续传输", exact: true }),
    ).toBeEnabled();
    if (retryStatus === "INTERRUPTED")
      await expect(row.getByText("重试未完成", { exact: true })).toBeVisible();
    await actions
      .getByRole("button", { name: "继续传输", exact: true })
      .dblclick();
    await expect(
      actions.getByRole("button", { name: "处理中…", exact: true }),
    ).toBeDisabled();
    expect(sent).toBe(1);
  });
}
