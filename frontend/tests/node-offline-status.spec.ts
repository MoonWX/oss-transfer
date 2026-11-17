import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

// The Master does not ask an Agent that is not connected and marks it
// offline. A connected Agent whose status read failed is a different case:
// it is online, and only this read failed.
test("an offline Agent and a connected Agent whose status read failed are told apart", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/nodes", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          nodes: [
            {
              address: "standby.example",
              label: "Standby",
              agent: true,
              online: false,
              lastSeen: "2026-11-16T08:00:00Z",
            },
            {
              address: "busy.example",
              label: "Busy",
              agent: true,
              online: true,
            },
          ],
          port: 1685,
        },
      },
    }),
  );
  await page.route("**/api/master/worker-status", (route) =>
    route.fulfill({
      json: {
        success: true,
        partial: true,
        failedWorkers: ["busy.example", "standby.example"],
        workers: {
          "standby.example": {
            success: false,
            offline: true,
            message: "节点离线",
          },
          "busy.example": {
            success: false,
            message: "context deadline exceeded",
          },
        },
      },
    }),
  );
  await page.goto("/nodes");
  const standby = page.getByRole("region", {
    name: "执行节点 Standby",
    exact: true,
  });
  const busy = page.getByRole("region", { name: "执行节点 Busy", exact: true });
  await expect(
    standby.getByText("节点离线，重新连接后显示运行状态。", { exact: true }),
  ).toBeVisible();
  await expect(standby.getByText("未能读取节点状态")).toHaveCount(0);
  await expect(busy.getByText("在线", { exact: true })).toBeVisible();
  await expect(
    busy.getByText("未能读取节点状态", { exact: true }),
  ).toBeVisible();
  await expect(busy.getByText("请求超时", { exact: true })).toBeVisible();
  await expect(busy.getByText("context deadline exceeded")).toHaveCount(0);
  await expect(busy.getByText(/节点离线/)).toHaveCount(0);

  await busy.getByRole("button", { name: "编辑节点", exact: true }).click();
  let dialog = page.getByRole("dialog", { name: "编辑节点", exact: true });
  await expect(
    dialog.getByText("未能读取节点当前的并发设置，请刷新后再调整。", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(dialog.getByText("节点离线时无法调整并发。")).toHaveCount(0);
  await dialog.getByRole("button", { name: "取消", exact: true }).click();
  await expect(dialog).toBeHidden();

  await standby.getByRole("button", { name: "编辑节点", exact: true }).click();
  dialog = page.getByRole("dialog", { name: "编辑节点", exact: true });
  await expect(
    dialog.getByText("节点离线时无法调整并发。", { exact: true }),
  ).toBeVisible();
});
