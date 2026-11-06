import { expect, test } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

function gate() {
  let release!: () => void;
  const promise = new Promise<void>((resolve) => {
    release = resolve;
  });
  return { promise, release };
}
// A running OSS-to-OSS task, like #107: one finished child has a failed object.
const runningTask = (failed = 1) => ({
  ...tasks[0],
  taskIndex: 1,
  generation: 3,
  status: "RUNNING",
  failedObjectCount: failed,
});

test("a running task retries its failed objects in place from the task details", async ({
  page,
}) => {
  await mockApi(page);
  // The default one-second refresh delivers the snapshot that ends the lock.
  let task = runningTask();
  const submitted = gate();
  let sent = 0;
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: 1, tasks: [task] } }),
  );
  await page.route("**/api/master/tasks/1", (route) =>
    route.fulfill({ json: { success: true, data: task } }),
  );
  await page.route("**/api/master/tasks/1/retry-failed", async (route) => {
    sent++;
    expect(route.request().method()).toBe("POST");
    expect(route.request().postDataJSON()).toEqual({ expectedGeneration: 3 });
    await submitted.promise;
    await route.fulfill({
      json: {
        success: true,
        data: {
          taskId: 1,
          status: "RUNNING",
          generation: 3,
          accepted: 1,
          ignored: 0,
          failed: 0,
          retriedObjects: 1,
          hasMore: false,
          children: [
            {
              childTaskId: "1-269",
              subtaskNumber: 269,
              failedObjectCount: 1,
              outcome: "QUEUED",
            },
          ],
        },
      },
    });
  });
  try {
    await page.goto("/tasks?taskId=1");
    const detail = page.locator(".MuiDrawer-paper");
    // The transfer keeps running: stopping stays available beside the retry.
    await expect(
      detail.getByRole("button", { name: "停止任务", exact: true }),
    ).toBeEnabled();
    const retry = detail.getByRole("button", {
      name: "重试失败项",
      exact: true,
    });
    await expect(retry).toBeEnabled();
    await retry.click();
    const dialog = page.getByRole("dialog", {
      name: "重试运行中任务的失败项？",
    });
    await expect(dialog).toContainText("正在进行的传输不会中断");
    await expect(dialog).toContainText("当前共 1 个失败对象");
    await dialog
      .getByRole("button", { name: "确认重试", exact: true })
      .dblclick();
    await expect(
      dialog.getByRole("button", { name: "提交中…", exact: true }),
    ).toBeDisabled();
    submitted.release();
    await expect(dialog).toBeHidden();
    await expect(
      page.getByText(
        "已开始重试任务 #1 的 1 个失败对象（1 个子任务），正在进行的传输不受影响。",
      ),
    ).toBeVisible();
    // Locked until a fresh snapshot shows the retry.
    await expect(
      detail.getByRole("button", { name: "重试中…", exact: true }),
    ).toBeDisabled();
    expect(sent).toBe(1);
    task = runningTask(0);
    await expect(
      detail.getByRole("button", { name: /^(重试失败项|重试中…)$/ }),
    ).toHaveCount(0);
    expect(sent).toBe(1);
  } finally {
    submitted.release();
  }
});

test("the exceptions page explains a refused running retry and permits another attempt", async ({
  page,
}) => {
  await mockApi(page);
  const task = runningTask(2);
  const reason = "请先升级执行节点 backup，才能在任务运行时重试失败项";
  let sent = 0;
  await page.route("**/api/master/tasks/1", (route) =>
    route.fulfill({ json: { success: true, data: task } }),
  );
  await page.route("**/api/master/local-transfers/failures?**", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          items: [
            {
              parentTaskId: 1,
              taskIndex: 1,
              childTaskId: "1-269",
              key: "failed.bin",
              errorMessage: "S3: HTTP 503 SlowDown",
              direction: "LOCAL_TO_OSS",
            },
          ],
          total: 1,
        },
      },
    }),
  );
  await page.route("**/api/master/tasks/1/retry-failed", (route) => {
    sent++;
    return route.fulfill({
      json: {
        success: true,
        data: {
          taskId: 1,
          status: "RUNNING",
          generation: 3,
          accepted: 0,
          ignored: 0,
          failed: 1,
          retriedObjects: 0,
          message: reason,
          children: [],
        },
      },
    });
  });
  // The ended-task retry (a new generation) must not be used for a running task.
  let resumed = 0;
  await page.route("**/api/master/retry", (route) => {
    resumed++;
    return route.fulfill({
      status: 500,
      json: { message: "unexpected resume" },
    });
  });
  await page.goto("/exceptions?type=local&taskId=1");
  const submit = async () => {
    const retry = page.getByRole("button", { name: "重试失败项", exact: true });
    await expect(retry).toBeEnabled();
    await retry.click();
    const dialog = page.getByRole("dialog", {
      name: "重试运行中任务的失败项？",
    });
    await dialog.getByRole("button", { name: "确认重试", exact: true }).click();
    await expect(dialog).toBeHidden();
    await expect(
      page.getByRole("alert").filter({ hasText: reason }).first(),
    ).toBeVisible();
  };
  await submit();
  await submit();
  expect(sent).toBe(2);
  expect(resumed).toBe(0);
});
