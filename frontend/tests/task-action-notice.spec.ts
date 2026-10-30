import { expect, test } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

for (const mode of ["light", "dark"] as const) {
  for (const route of ["/", "/tasks"]) {
    test(`${mode} ${route} resume leaves one notice without a permanent result panel`, async ({
      page,
    }) => {
      await mockApi(page);
      await page.addInitScript((theme) => {
        localStorage.setItem("oss-transfer-theme", theme);
        localStorage.setItem("oss-transfer-refresh-seconds", "0");
      }, mode);
      const task = { ...tasks[2], status: "STOPPED", failedObjectCount: 0 };
      await page.route("**/api/master/status", (request) =>
        request.fulfill({ json: { tasks: [task] } }),
      );
      let submissions = 0;
      await page.route("**/api/master/local-transfers/resume", (request) => {
        submissions++;
        task.status = "RUNNING";
        task.generation++;
        return request.fulfill({
          json: {
            success: true,
            accepted: 1,
            details: [
              {
                taskId: task.taskIndex,
                success: true,
                generation: task.generation,
              },
            ],
          },
        });
      });
      await page.goto(`${route}?taskId=${task.taskIndex}`);
      await page.getByRole("button", { name: "继续传输", exact: true }).click();
      await page
        .locator(".MuiDialog-root [role=dialog]")
        .getByRole("button", { name: "确认继续传输", exact: true })
        .click();
      await expect(page.locator(".MuiDialog-root [role=dialog]")).toHaveCount(
        0,
      );
      await expect(page.locator(".MuiSnackbar-root")).toContainText(
        "已受理 1，忽略 0，失败 0",
      );
      await expect(
        page.locator(".MuiDrawer-paper").getByText("执行中", { exact: true }),
      ).toBeVisible();
      await page.getByRole("button", { name: "关闭详情", exact: true }).click();
      await expect(page.locator("main").getByRole("alert")).toHaveCount(0);
      await expect(page.locator("main").getByLabel("批量操作进度")).toHaveCount(
        0,
      );
      expect(submissions).toBe(1);
    });
  }

  test(`${mode} overview keeps a bordered failure notice without a finished progress bar`, async ({
    page,
  }, testInfo) => {
    await mockApi(page);
    await page.addInitScript((theme) => {
      localStorage.setItem("oss-transfer-theme", theme);
      localStorage.setItem("oss-transfer-refresh-seconds", "0");
    }, mode);
    await page.route("**/api/master/stop", (request) =>
      request.abort("failed"),
    );
    await page.goto("/?taskId=1048");
    await page.getByRole("button", { name: "停止任务", exact: true }).click();
    await page
      .locator(".MuiDialog-root [role=dialog]")
      .getByRole("button", { name: "确认停止", exact: true })
      .click();
    await expect(page.locator(".MuiDialog-root [role=dialog]")).toHaveCount(0);
    await page.getByRole("button", { name: "关闭详情", exact: true }).click();
    const notice = page.locator("main").getByRole("alert");
    await expect(notice).toContainText("待确认 1，未提交 0");
    await expect(notice).toHaveCSS("border-top-width", "1px");
    await expect(notice).toHaveCSS("border-top-style", "solid");
    await expect(notice).toHaveCSS("border-radius", "24px");
    await expect(notice).toHaveCSS(
      "background-color",
      mode === "dark" ? "rgb(28, 32, 38)" : "rgb(255, 255, 255)",
    );
    await expect(notice.getByRole("progressbar")).toHaveCount(0);
    await notice.getByText("查看失败详情", { exact: true }).click();
    await expect(notice).toContainText("#1048:");
    await notice.screenshot({
      path: testInfo.outputPath(`notice-${mode}.png`),
    });
    await notice.getByRole("button", { name: "关闭", exact: true }).click();
    await expect(notice).toHaveCount(0);
  });
}
