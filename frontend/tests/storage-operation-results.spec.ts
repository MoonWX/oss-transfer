import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

for (const operation of [
  { action: "添加", method: "POST" },
  { action: "更新", method: "PUT" },
  { action: "删除", method: "DELETE" },
]) {
  for (const partial of [false, true]) {
    test(`${operation.method} names its action consistently for ${partial ? "partial failure" : "both successful nodes"}`, async ({
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
            successCount: partial ? 1 : 2,
            failedCount: partial ? 1 : 0,
            message: `${operation.action}结果：成功 ${partial ? 1 : 2} 个节点，失败 ${partial ? 1 : 0} 个节点`,
            details: [
              `10.0.1.12: ${operation.action}成功`,
              `10.0.1.13: ${operation.action}${partial ? "失败" : "成功"}`,
            ],
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
      await expect(result).toContainText(
        partial
          ? `部分节点${operation.action}失败`
          : `连接${operation.action}完成`,
      );
      await expect(result).toContainText(
        `${operation.action}结果：成功 ${partial ? 1 : 2} 个节点，失败 ${partial ? 1 : 0} 个节点`,
      );
      await expect(result.getByRole("listitem")).toHaveText([
        `10.0.1.12: ${operation.action}成功`,
        `10.0.1.13: ${operation.action}${partial ? "失败" : "成功"}`,
      ]);
      await expect(result).toHaveClass(
        partial ? /MuiAlert-standardWarning/ : /MuiAlert-standardSuccess/,
      );
      await expect(result).not.toContainText("同步");
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
        partial ? /MuiAlert-standardWarning/ : /MuiAlert-standardSuccess/,
      );
      await expect(result).toContainText(
        partial
          ? "一部のノードで設定を保存できませんでした"
          : "失败-archive: 保存済み",
      );
    });
  }
});
