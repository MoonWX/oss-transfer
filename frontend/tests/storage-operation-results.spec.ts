import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

// The Master saves the change first; a node that did not take it within the
// request is synced before it next works, as the Master's note says. That is
// not a failure, and a deletion is announced as a deletion.
const syncNote = "未同步的节点会在下次执行任务或对象比对前自动同步。";
for (const operation of [
  { action: "添加", method: "POST", message: "存储连接已保存" },
  { action: "更新", method: "PUT", message: "存储连接已保存" },
  { action: "删除", method: "DELETE", message: "存储连接已删除" },
]) {
  for (const partial of [false, true]) {
    test(`${operation.method} names its action ${partial ? "and says when a node awaiting sync receives it" : "with both nodes synced"}`, async ({
      page,
    }) => {
      await mockApi(page);
      let mutations = 0;
      await page.route("**/api/master/s3-config**", async (route) => {
        if (route.request().method() === "GET") return route.fallback();
        expect(route.request().method()).toBe(operation.method);
        mutations++;
        await route.fulfill({
          json: {
            success: true,
            partial,
            revision: 4,
            successCount: partial ? 1 : 2,
            failedCount: partial ? 1 : 0,
            message: operation.message,
            details: [
              "10.0.1.12: 同步成功",
              `10.0.1.13: ${partial ? "等待同步" : "同步成功"}`,
            ],
            ...(partial ? { syncNote } : {}),
          },
        });
      });
      await page.goto("/storage");
      if (operation.method === "POST") {
        await page
          .getByRole("button", { name: "添加连接", exact: true })
          .click();
        const dialog = page.getByRole("dialog");
        await dialog.getByLabel("连接名称").fill("new-test");
        await dialog.getByLabel("Endpoint").fill("https://test.example");
        await dialog.getByLabel("Access Key").fill("FAKE_KEY");
        await dialog.getByLabel("Secret Key").fill("FAKE_SECRET");
      } else {
        await page
          .getByRole("button", {
            name: `${operation.method === "PUT" ? "编辑" : "删除"}连接 production`,
            exact: true,
          })
          .click();
      }
      await page
        .getByRole("dialog")
        .getByRole("button", {
          name: operation.method === "DELETE" ? "删除连接" : "保存连接",
          exact: true,
        })
        .click();
      await expect(page.getByRole("dialog")).toBeHidden();
      const result = page
        .getByRole("alert")
        .filter({ has: page.getByRole("list") });
      await expect(result).toContainText(`连接${operation.action}完成`);
      await expect(result).toContainText(operation.message);
      await expect(result).not.toContainText("失败");
      await expect(result.getByRole("listitem")).toHaveText([
        "10.0.1.12: 同步成功",
        `10.0.1.13: ${partial ? "等待同步" : "同步成功"}`,
      ]);
      if (partial) await expect(result).toContainText(syncNote);
      else await expect(result).not.toContainText("下次执行任务");
      await expect(result).toHaveClass(
        partial ? /MuiAlert-standardInfo/ : /MuiAlert-standardSuccess/,
      );
      // The notification repeats the Master's message, not a failure.
      await expect(
        page
          .getByRole("alert")
          .filter({ hasNot: page.getByRole("list") })
          .filter({ hasText: operation.message }),
      ).toBeVisible();
      expect(mutations).toBe(1);
    });
  }
}

test.describe("storage result status does not depend on its language", () => {
  test.use({ locale: "ja-JP" });
  for (const partial of [true, false]) {
    test(`uses structured status for ${partial ? "Japanese failure" : "a success with a failure-like node name"}`, async ({
      page,
    }) => {
      await mockApi(page);
      await page.route("**/api/master/s3-config**", async (route) => {
        if (route.request().method() === "GET") return route.fallback();
        await route.fulfill({
          json: {
            success: true,
            ...(partial ? { failedCount: 1 } : {}),
            message: partial
              ? "一部のノードで設定を保存できませんでした"
              : "保存しました",
            // Node names are data, including names that contain Chinese failure words.
            details: partial
              ? ["ノード a: 応答なし"]
              : ["失败-archive: 保存済み"],
          },
        });
      });
      await page.goto("/storage");
      await page
        .getByRole("button", { name: "接続 production を編集", exact: true })
        .click();
      await page
        .getByRole("dialog")
        .getByRole("button", { name: "接続を保存", exact: true })
        .click();
      await expect(page.getByRole("dialog")).toBeHidden();
      const result = page
        .getByRole("alert")
        .filter({ has: page.getByRole("list") });
      await expect(result).toHaveClass(
        partial ? /MuiAlert-standardInfo/ : /MuiAlert-standardSuccess/,
      );
      await expect(result).toContainText(
        partial
          ? "一部のノードで設定を保存できませんでした"
          : "失败-archive: 保存済み",
      );
    });
  }
});
