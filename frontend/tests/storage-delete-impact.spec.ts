import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

// Tasks keep only the connection name and open storage from it for every
// subtask, so deleting a connection stops the tasks that use it; the dialog
// must not suggest that only future transfers are affected.
for (const [locale, button, impact] of [
  [
    "zh-CN",
    "删除连接 production",
    "删除连接 production 后，所有使用此连接的任务都将无法继续执行（包括执行中和已停止的任务），直到重新添加同名连接；后续迁移也无法使用此连接。",
  ],
  [
    "en-US",
    "Delete connection production",
    "every task that uses this connection, running or stopped, cannot continue until a connection with the same name is added again",
  ],
] as const) {
  test(`deleting a connection says what happens to the tasks that use it (${locale})`, async ({
    page,
  }) => {
    await page.addInitScript((value) => {
      localStorage.setItem("oss-transfer-language", value);
    }, locale);
    await mockApi(page);
    await page.goto("/storage");
    await page.getByRole("button", { name: button, exact: true }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toContainText(impact);
    await expect(dialog).not.toContainText("后续迁移将无法使用此连接。此操作");
  });
}
