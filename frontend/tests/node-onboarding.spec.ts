import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

test("node name is primary and machine details load only when opened", async ({
  page,
}) => {
  await mockApi(page);
  const id = "hW_g_ip1GDvhDh3xBwMFulcpdOv3deSLtRgeOeWYhI4";
  let detailsCalls = 0;
  await page.route("**/api/master/nodes", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          nodes: [
            { id, address: id, label: "存储节点一", agent: true, online: true },
          ],
          port: 1685,
        },
      },
    }),
  );
  await page.route("**/api/master/worker-status", (route) =>
    route.fulfill({
      json: {
        workers: { [id]: { success: true } },
      },
    }),
  );
  await page.route("**/api/master/nodes/*/details", (route) => {
    detailsCalls++;
    return route.fulfill({
      json: {
        success: true,
        data: {
          id,
          address: id,
          label: "存储节点一",
          online: true,
          complete: true,
          hostname: "storage-01",
          os: "linux",
          arch: "amd64",
          localIps: ["10.1.0.170"],
          connectionAddress: "10.1.0.170",
          advertisedLocalRoots: ["/mnt/cfs"],
          mounts: [
            {
              path: "/mnt/cfs",
              source: "10.1.0.232:/cfs",
              fileSystem: "nfs4",
              readOnly: false,
            },
          ],
        },
      },
    });
  });
  await page.goto("/nodes");
  await expect(page.getByRole("heading", { name: "存储节点一" })).toBeVisible();
  await expect(page.getByText(id, { exact: true })).toHaveCount(0);
  expect(detailsCalls).toBe(0);
  await page.getByRole("button", { name: "节点详情", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "节点详情 · 存储节点一" });
  await expect(dialog.getByText("storage-01", { exact: true })).toBeVisible();
  await expect(
    dialog.getByText("10.1.0.232:/cfs", { exact: true }),
  ).toBeVisible();
  await expect(dialog.getByText(id, { exact: true })).toBeVisible();
  expect(detailsCalls).toBeGreaterThan(0);
});

test("node invitation uses the current origin by default and accepts a private endpoint", async ({
  page,
}) => {
  const requests = await mockApi(page);
  const invitations: unknown[] = [];
  await page.route("**/api/master/agents/invitations", async (route) => {
    invitations.push(route.request().postDataJSON());
    await route.fulfill({
      json: {
        success: true,
        data: {
          id: "test-invitation",
          joinUrl: "http://10.1.0.43:1685/api/agents/bootstrap/test",
          traditionalCommand:
            "curl -fsSL 'http://10.1.0.43:1685/api/agents/bootstrap/test' | bash",
          expiresAt: new Date(Date.now() + 1800000).toISOString(),
        },
      },
    });
  });
  await page.goto("/nodes");
  await expect(
    page.getByRole("button", { name: "连接地址", exact: true }),
  ).toHaveCount(0);
  expect(
    requests.some((request) => request.path === "/api/master/network"),
  ).toBe(false);
  await page.getByRole("button", { name: "添加节点", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "添加节点", exact: true });
  await dialog.getByLabel("节点名称").fill("storage-02");
  await expect(dialog.getByLabel("Agent 连接地址")).toHaveValue(
    new URL(page.url()).origin,
  );
  await dialog.getByLabel("Agent 连接地址").fill("http://10.1.0.43:1685/path");
  await dialog
    .getByRole("button", { name: "生成加入指令", exact: true })
    .click();
  await expect(
    dialog.getByText("请输入完整的 HTTP 或 HTTPS 地址，不含路径。"),
  ).toBeVisible();
  expect(invitations).toHaveLength(0);
  await dialog.getByLabel("Agent 连接地址").fill("http://10.1.0.43:1685/");
  await dialog
    .getByRole("button", { name: "生成加入指令", exact: true })
    .click();
  await expect(dialog.getByLabel("传统部署 指令", { exact: true })).toHaveValue(
    /10\.1\.0\.43:1685/,
  );
  expect(invitations).toEqual([
    { label: "storage-02", masterUrl: "http://10.1.0.43:1685" },
  ]);
  await dialog.getByRole("button", { name: "关闭", exact: true }).click();
  await page.getByRole("button", { name: "添加节点", exact: true }).click();
  await expect(dialog.getByLabel("Agent 连接地址")).toHaveValue(
    new URL(page.url()).origin,
  );
  await dialog.getByLabel("节点名称").fill("storage-03");
  await dialog.getByLabel("Agent 连接地址").fill("");
  await dialog
    .getByRole("button", { name: "生成加入指令", exact: true })
    .click();
  await expect(
    dialog.getByLabel("传统部署 指令", { exact: true }),
  ).toBeVisible();
  expect(invitations[1]).toEqual({
    label: "storage-03",
    masterUrl: new URL(page.url()).origin,
  });
});
