import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

// One online node runs the connection test and the validation on save, while
// a task may run on every node. Each result names that node.
const node = "Office LAN (10.0.0.8)";

test("a connection test names the node that ran it, when it passes and when it fails", async ({
  page,
}) => {
  await mockApi(page);
  let fail = false;
  await page.route("**/api/master/s3-config/production/buckets", (route) =>
    fail
      ? route.fulfill({
          status: 502,
          json: {
            success: false,
            message:
              "连接 10.0.0.5:9000 超时，请检查执行节点到 Endpoint 的网络和防火墙。",
            reasonCode: "STORAGE_TIMEOUT",
            node,
          },
        })
      : route.fulfill({
          json: {
            success: true,
            buckets: ["data"],
            pathStyle: true,
            pathStyleAuto: true,
            checkedBucket: "data",
            node,
          },
        }),
  );
  await page.goto("/storage");
  const test = page.getByRole("button", { name: "测试连接", exact: true });
  await test.first().click();
  let dialog = page.getByRole("dialog");
  await expect(dialog.getByRole("alert")).toHaveText("已获取 1 个存储桶。");
  await expect(dialog.getByTestId("storage-check-node")).toHaveText(
    `由执行节点 ${node} 检查。`,
  );
  await dialog.getByRole("button", { name: "关闭", exact: true }).click();
  await expect(dialog).toHaveCount(0);

  fail = true;
  await test.first().click();
  dialog = page.getByRole("dialog");
  const failure = dialog.getByRole("alert");
  await expect(failure).toContainText(
    "连接 10.0.0.5:9000 超时，请检查执行节点到 Endpoint 的网络和防火墙。",
  );
  await expect(failure.getByTestId("storage-check-node")).toHaveText(
    `由执行节点 ${node} 检查。`,
  );
});

test("saving with validation names the node that validated the connection", async ({
  page,
}) => {
  await mockApi(page);
  let fail = true;
  await page.route("**/api/master/s3-config", (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    return fail
      ? route.fulfill({
          status: 400,
          json: {
            success: false,
            message:
              "存储验证失败：连接 10.0.0.5:9000 超时，请检查执行节点到 Endpoint 的网络和防火墙。",
            reasonCode: "STORAGE_TIMEOUT",
            node,
          },
        })
      : route.fulfill({
          json: {
            success: true,
            message: "存储连接已保存",
            successCount: 1,
            failedCount: 0,
            partial: false,
            details: [`${node}: 同步成功`],
            validationNode: node,
          },
        });
  });
  await page.goto("/storage");
  await page.getByRole("button", { name: "添加连接", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel("连接名称").fill("lan-minio");
  await dialog.getByLabel("Endpoint").fill("http://10.0.0.5:9000");
  await dialog.getByLabel("Access Key").fill("FAKE_KEY");
  await dialog.getByLabel("Secret Key").fill("FAKE_SECRET");
  await dialog.getByRole("button", { name: "保存连接", exact: true }).click();
  const failure = dialog.getByRole("alert");
  await expect(failure).toContainText("存储验证失败：连接 10.0.0.5:9000 超时");
  await expect(failure.getByTestId("storage-check-node")).toHaveText(
    `由执行节点 ${node} 检查。`,
  );

  fail = false;
  await dialog.getByRole("button", { name: "保存连接", exact: true }).click();
  await expect(dialog).toBeHidden();
  const result = page
    .getByRole("alert")
    .filter({ has: page.getByRole("list") });
  await expect(result).toContainText(`保存前已由执行节点 ${node} 验证连接。`);
});
