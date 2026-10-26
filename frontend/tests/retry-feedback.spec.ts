import { expect, test } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

function gate() {
  let release!: () => void;
  const promise = new Promise<void>((resolve) => {
    release = resolve;
  });
  return { promise, release };
}

for (const accepted of [true, false]) {
  test(`resume ${accepted ? "acceptance" : "failure"} unlocks controls before a stalled task refresh`, async ({
    page,
  }) => {
    await mockApi(page);
    await page.addInitScript(() =>
      localStorage.setItem("oss-transfer-refresh-seconds", "0"),
    );
    const task = { ...tasks[2], status: "PARTIAL_FAILED", taskIndex: 1 };
    const submitted = gate();
    const refreshed = gate();
    let sent = 0;
    let refreshing = false;
    await page.route("**/api/master/status", async (route) => {
      if (refreshing) await refreshed.promise;
      await route.fulfill({ json: { totalTasks: 1, tasks: [task] } });
    });
    await page.route("**/api/master/local-transfers/resume", async (route) => {
      sent++;
      expect(route.request().postDataJSON()).toEqual({ taskIds: [1] });
      await submitted.promise;
      refreshing = true;
      await route.fulfill(
        accepted
          ? {
              json: {
                success: true,
                data: { accepted: 1, ignored: 0, failed: 0 },
              },
            }
          : { status: 503, json: { message: "恢复暂不可用" } },
      );
    });
    try {
      await page.goto("/tasks?taskId=1");
      await page.getByRole("button", { name: "继续传输", exact: true }).click();
      const dialog = page.getByRole("dialog", { name: "继续传输 1 个任务？" });
      await dialog
        .getByRole("button", { name: "确认继续传输", exact: true })
        .dblclick();
      await expect(
        dialog.getByRole("button", { name: "提交中…", exact: true }),
      ).toBeDisabled();
      expect(sent).toBe(1);
      submitted.release();
      await expect(dialog).toBeHidden();
      // The refresh remains unresolved here; it must not hold the action lock
      // or postpone the accepted/error result until another network timeout.
      await expect(
        page.getByRole("button", { name: "继续传输", exact: true }),
      ).toBeEnabled();
      if (accepted) {
        await expect(page.getByText(/^继续传输请求已提交/)).toBeVisible();
      } else {
        await expect(
          page
            .getByRole("alert")
            .filter({ hasText: "部分请求未完成：恢复暂不可用" })
            .first(),
        ).toBeVisible();
      }
      expect(sent).toBe(1);
    } finally {
      submitted.release();
      refreshed.release();
    }
  });
}

test("exception retry shows submission and a durable task link without waiting for records to refresh", async ({
  page,
}) => {
  await mockApi(page);
  const submitted = gate();
  const refreshed = gate();
  let sent = 0;
  let refreshing = false;
  const failure = {
    parentTaskId: 1,
    taskIndex: 1,
    key: "failed.bin",
    errorMessage: "S3: HTTP 400",
    direction: "LOCAL_TO_OSS",
  };
  await page.route(
    "**/api/master/local-transfers/failures?**",
    async (route) => {
      if (refreshing) await refreshed.promise;
      await route.fulfill({
        json: {
          success: true,
          data: {
            items: [failure, { ...failure, key: "another.bin" }],
            total: 2,
          },
        },
      });
    },
  );
  await page.route(
    "**/api/master/local-transfers/retry-failures",
    async (route) => {
      sent++;
      expect(route.request().postDataJSON()).toEqual({ taskIds: [1] });
      await submitted.promise;
      refreshing = true;
      await route.fulfill({
        json: { success: true, data: { accepted: 1, ignored: 0, failed: 0 } },
      });
    },
  );
  try {
    await page.goto("/exceptions?type=local&taskId=1");
    await page
      .getByRole("button", { name: "重试任务", exact: true })
      .first()
      .dblclick();
    const buttons = page.getByRole("button", { name: "提交中…", exact: true });
    await expect(buttons).toHaveCount(2);
    await expect(buttons.first()).toBeDisabled();
    await expect(buttons.last()).toBeDisabled();
    expect(sent).toBe(1);
    submitted.release();
    const result = page
      .getByRole("alert")
      .filter({ hasText: "任务 #1 的重试已受理。" });
    await expect(result).toBeVisible();
    await expect(
      result.getByRole("link", { name: "查看任务" }),
    ).toHaveAttribute("href", "/tasks?taskId=1");
    await expect(
      page.getByRole("button", { name: "完整迁移报告" }),
    ).toBeEnabled();
    await expect(
      page.getByRole("tab", { name: "失败对象", exact: true }),
    ).toBeEnabled();
    expect(sent).toBe(1);
  } finally {
    submitted.release();
    refreshed.release();
  }
});

for (const surface of ["tasks", "exceptions"] as const) {
  test(`${surface} shows the actual rejection inside a successful HTTP response`, async ({
    page,
  }) => {
    await mockApi(page);
    const task = { ...tasks[2], status: "PARTIAL_FAILED", taskIndex: 1 };
    const reason = "节点 backup 已被移除，请重新添加节点后创建任务。";
    await page.route("**/api/master/status", (route) =>
      route.fulfill({ json: { totalTasks: 1, tasks: [task] } }),
    );
    await page.route("**/api/master/local-transfers/failures?**", (route) =>
      route.fulfill({
        json: {
          success: true,
          data: {
            total: 1,
            items: [
              { parentTaskId: 1, key: "failed.bin", errorMessage: "HTTP 400" },
            ],
          },
        },
      }),
    );
    const action = surface === "tasks" ? "resume" : "retry-failures";
    await page.route(`**/api/master/local-transfers/${action}`, (route) =>
      route.fulfill({
        json: {
          success: true,
          data: {
            accepted: 0,
            ignored: 0,
            failed: 1,
            partial: true,
            details: [
              {
                taskId: 1,
                status: "PARTIAL_FAILED",
                success: false,
                message: reason,
              },
            ],
          },
        },
      }),
    );
    await page.goto(
      surface === "tasks"
        ? "/tasks?taskId=1"
        : "/exceptions?type=local&taskId=1",
    );
    if (surface === "tasks") {
      await page.getByRole("button", { name: "继续传输", exact: true }).click();
      await page
        .getByRole("button", { name: "确认继续传输", exact: true })
        .click();
      await expect(
        page.getByRole("dialog", { name: "继续传输 1 个任务？" }),
      ).toBeHidden();
      await expect(
        page.getByRole("button", { name: "继续传输", exact: true }),
      ).toBeEnabled();
    } else {
      await page.getByRole("button", { name: "重试任务", exact: true }).click();
      await expect(
        page.getByRole("button", { name: "重试任务", exact: true }),
      ).toBeEnabled();
    }
    await expect(
      page.getByRole("alert").filter({ hasText: reason }).first(),
    ).toBeVisible();
    await expect(page.getByText(/不可达节点：未知/)).toHaveCount(0);
    await expect(
      page.getByText("任务 #1 的重试已受理。", { exact: true }),
    ).toHaveCount(0);
    await expect(page.getByText(/^继续传输请求已提交/)).toHaveCount(0);
  });
}
