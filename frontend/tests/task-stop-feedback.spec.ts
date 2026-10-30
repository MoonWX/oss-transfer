import { expect, test } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

function gate() {
  let release!: () => void;
  const promise = new Promise<void>((resolve) => {
    release = resolve;
  });
  return { promise, release };
}

for (const local of [false, true]) {
  test(`${local ? "local" : "OSS"} stop closes confirmation before the response and prevents duplicate submissions`, async ({
    page,
  }) => {
    await mockApi(page);
    await page.addInitScript(() =>
      localStorage.setItem("oss-transfer-refresh-seconds", "0"),
    );
    const task = { ...tasks[local ? 2 : 0], status: "RUNNING" };
    const response = gate();
    let requests = 0;
    await page.route("**/api/master/status", (route) =>
      route.fulfill({ json: { tasks: [task] } }),
    );
    await page.route(
      local ? "**/api/master/local-transfers/stop" : "**/api/master/stop",
      async (route) => {
        requests++;
        expect(route.request().postDataJSON()).toEqual({
          taskIds: [task.taskIndex],
        });
        await response.promise;
        task.status = "STOPPING";
        await route.fulfill({
          json: {
            success: true,
            accepted: 1,
            details: [
              { taskId: task.taskIndex, success: true, status: "STOPPING" },
            ],
          },
        });
      },
    );
    try {
      await page.goto(`/tasks?taskId=${task.taskIndex}`);
      await page.getByRole("button", { name: "停止任务", exact: true }).click();
      await page
        .locator(".MuiDialog-root [role=dialog]")
        .getByRole("button", { name: "确认停止", exact: true })
        .evaluate((button: HTMLButtonElement) => {
          button.click();
          button.click();
        });
      await expect(page.locator(".MuiDialog-root [role=dialog]")).toHaveCount(
        0,
      );
      await expect(
        page.getByRole("button", { name: "停止任务", exact: true }),
      ).toBeDisabled();
      expect(requests).toBe(1);
      await page.getByRole("button", { name: "关闭详情", exact: true }).click();
      await expect(
        page.locator("main").getByText("正在提交停止请求…", { exact: true }),
      ).toBeVisible();
      await expect(page.getByText(/停止请求已提交/)).toHaveCount(0);
      await expect(
        page.locator("main").getByText("已停止", { exact: true }),
      ).toHaveCount(0);
      // Inspecting another part of the page stays possible while the request is pending.
      await page
        .getByRole("button", {
          name: `查看任务 ${task.taskIndex}`,
          exact: true,
        })
        .click();
      await expect(
        page.getByRole("heading", {
          name: `迁移任务 #${task.taskIndex}`,
          exact: true,
        }),
      ).toBeVisible();
      response.release();
      await expect(page.getByText(/停止请求已提交，已受理 1/)).toBeVisible();
      await expect(
        page.locator(".MuiDrawer-paper").getByText("停止中", { exact: true }),
      ).toBeVisible();
      await expect(
        page.getByRole("button", { name: "停止任务", exact: true }),
      ).toHaveCount(0);
      expect(requests).toBe(1);
    } finally {
      response.release();
    }
  });
}

test("a lost stop response remains visible outside the closed confirmation and is not automatically replayed", async ({
  page,
}) => {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "0"),
  );
  const response = gate();
  let requests = 0;
  await page.route("**/api/master/stop", async (route) => {
    requests++;
    await response.promise;
    await route.abort("failed");
  });
  try {
    await page.goto("/tasks?taskId=1048");
    await page.getByRole("button", { name: "停止任务", exact: true }).click();
    await page
      .locator(".MuiDialog-root [role=dialog]")
      .getByRole("button", { name: "确认停止", exact: true })
      .click();
    await expect(page.locator(".MuiDialog-root [role=dialog]")).toHaveCount(0);
    response.release();
    await expect(
      page
        .locator(".MuiDrawer-paper")
        .getByText(/部分请求未完成：无法连接服务/),
    ).toBeVisible();
    await page.getByRole("button", { name: "关闭详情", exact: true }).click();
    await expect(
      page.locator("main").getByText("待确认 1，未提交 0", { exact: true }),
    ).toBeVisible();
    await expect(page.getByText(/停止请求已提交/)).toHaveCount(0);
    await page.getByRole("button", { name: "刷新任务", exact: true }).click();
    expect(requests).toBe(1);
  } finally {
    response.release();
  }
});
