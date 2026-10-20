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

test("network settings save a private Agent URL without changing the web URL", async ({
  page,
}) => {
  await mockApi(page);
  let saved: unknown;
  await page.route("**/api/master/network", async (route) => {
    if (route.request().method() === "PUT")
      saved = route.request().postDataJSON();
    await route.fulfill({
      json: {
        success: true,
        data: {
          publicUrl: "https://transfer.example",
          agentUrl: "",
        },
      },
    });
  });
  await page.goto("/nodes");
  await page.getByRole("button", { name: "连接地址", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "连接地址" });
  await expect(dialog.getByLabel("浏览器访问地址")).toHaveValue(
    "https://transfer.example",
  );
  await expect(dialog.getByLabel("Agent 连接地址")).toHaveValue("");
  await dialog.getByLabel("Agent 连接地址").fill("http://10.1.0.43:1685/path");
  await dialog.getByRole("button", { name: "保存", exact: true }).click();
  await expect(
    dialog.getByText("请输入完整的 HTTP 或 HTTPS 地址，不含路径。"),
  ).toBeVisible();
  expect(saved).toBeUndefined();
  await dialog.getByLabel("Agent 连接地址").fill("http://10.1.0.43:1685");
  await dialog.getByRole("button", { name: "保存", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(saved).toEqual({
    publicUrl: "https://transfer.example",
    agentUrl: "http://10.1.0.43:1685",
  });
});

test("an inherited Agent address stays empty after reopening and changing the web URL", async ({
  page,
}) => {
  await mockApi(page);
  let settings = {
    publicUrl: "https://transfer.example",
    agentUrl: "http://10.1.0.43:1685",
  };
  await page.route("**/api/master/network", async (route) => {
    if (route.request().method() === "PUT")
      settings = route.request().postDataJSON();
    await route.fulfill({ json: { success: true, data: settings } });
  });
  await page.goto("/nodes");
  await page.getByRole("button", { name: "连接地址", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "连接地址" });
  await dialog.getByLabel("Agent 连接地址").fill("");
  await dialog.getByRole("button", { name: "保存", exact: true }).click();
  await expect(dialog).toBeHidden();
  await page.getByRole("button", { name: "连接地址", exact: true }).click();
  await expect(dialog.getByLabel("Agent 连接地址")).toHaveValue("");
  await dialog
    .getByLabel("浏览器访问地址")
    .fill("https://new.transfer.example");
  await expect(dialog.getByLabel("Agent 连接地址")).toHaveAttribute(
    "placeholder",
    "https://new.transfer.example",
  );
  await dialog.getByRole("button", { name: "保存", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(settings).toEqual({
    publicUrl: "https://new.transfer.example",
    agentUrl: "",
  });
});

for (const agentUrl of ["", "http://10.1.0.43:1685"]) {
  test(`setup preserves Agent address ${agentUrl || "inheritance"}`, async ({
    page,
  }) => {
    await mockApi(page);
    let body: unknown;
    await page.route("**/api/setup/status", (route) =>
      route.fulfill({
        json: {
          success: true,
          configured: false,
          tokenRequired: true,
        },
      }),
    );
    await page.route("**/api/setup/complete", (route) => {
      body = route.request().postDataJSON();
      return route.fulfill({
        status: 403,
        json: { success: false, message: "Invalid setup token" },
      });
    });
    await page.goto("/");
    await page.getByLabel(/^密码/).fill("only-a-test-password");
    await page.getByLabel(/^确认密码/).fill("only-a-test-password");
    await page.getByRole("button", { name: "下一步", exact: true }).click();
    await page.getByLabel("浏览器访问地址").fill("https://transfer.example");
    await page.getByLabel("Agent 连接地址").fill(agentUrl);
    await page.getByRole("button", { name: "下一步", exact: true }).click();
    await page.getByLabel("初始化密钥").fill("setup-token-test");
    await page.getByRole("button", { name: "完成并登录", exact: true }).click();
    await expect
      .poll(() => body)
      .toEqual({
        username: "admin",
        password: "only-a-test-password",
        publicUrl: "https://transfer.example",
        agentUrl,
        setupToken: "setup-token-test",
      });
  });
}
