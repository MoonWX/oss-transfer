import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

const pages = [
  { path: "/", link: "概览", heading: "迁移概览" },
  { path: "/tasks", link: "迁移任务", heading: "迁移任务" },
  { path: "/create", link: "创建迁移", heading: "创建迁移" },
  { path: "/storage", link: "存储连接", heading: "存储连接" },
  { path: "/nodes", link: "执行节点", heading: "执行节点" },
  { path: "/exceptions", link: "异常记录", heading: "异常记录" },
  { path: "/tools", link: "运维工具", heading: "运维工具" },
];

test("page navigation preserves the shell and document while updating clean URLs", async ({
  page,
}) => {
  await mockApi(page);
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "迁移概览" })).toBeVisible();
  const sidebar = await page
    .getByRole("link", { name: "存储连接", exact: true })
    .elementHandle();
  const toolbar = await page
    .getByRole("button", { name: "账号设置" })
    .elementHandle();
  await page.evaluate(() =>
    Object.assign(window, { shellDocumentMarker: "original-shell" }),
  );
  const documentRequests: string[] = [];
  page.on("request", (request) => {
    if (request.isNavigationRequest() && request.resourceType() === "document")
      documentRequests.push(request.url());
  });
  for (const destination of [...pages.slice(1), pages[0]]) {
    const link = page
      .getByRole("navigation", { name: "主导航" })
      .getByRole("link", {
        name: destination.link,
        exact: true,
      });
    await expect(link).toHaveAttribute("href", destination.path);
    await link.click();
    await expect(page).toHaveURL(destination.path);
    await expect(page).toHaveTitle(`${destination.link} · OSS Transfer`);
    await expect(
      page.getByRole("heading", {
        name: destination.heading,
        exact: true,
      }),
    ).toBeVisible();
    expect(
      await page.evaluate(() => Reflect.get(window, "shellDocumentMarker")),
    ).toBe("original-shell");
    expect(await sidebar!.evaluate((element) => element.isConnected)).toBe(
      true,
    );
    expect(await toolbar!.evaluate((element) => element.isConnected)).toBe(
      true,
    );
    await expect(page.locator("iframe")).toHaveCount(0);
    expect(new URL(page.url()).hash).toBe("");
  }
  expect(documentRequests).toEqual([]);
});

test("unknown routes show their own title and return to overview within the shell", async ({
  page,
}) => {
  await mockApi(page);
  await page.goto("/qa-missing-route");
  await expect(page.getByRole("alert")).toContainText("页面不存在");
  await expect(page).toHaveTitle("页面不存在 · OSS Transfer");
  await expect(page.getByRole("banner")).toContainText("页面不存在");
  await expect(page.getByRole("banner")).not.toContainText("创建迁移");
  const toolbar = await page
    .getByRole("button", { name: "账号设置" })
    .elementHandle();
  await page.getByRole("link", { name: "返回概览", exact: true }).click();
  await expect(page).toHaveURL("/");
  await expect(page).toHaveTitle("概览 · OSS Transfer");
  await expect(page.getByRole("heading", { name: "迁移概览" })).toBeVisible();
  expect(await toolbar!.evaluate((element) => element.isConnected)).toBe(true);
});

test("each clean page path supports direct opening and browser refresh", async ({
  page,
}) => {
  test.setTimeout(90_000);
  await mockApi(page);
  for (const destination of pages) {
    const response = await page.goto(destination.path);
    expect(response?.status()).toBe(200);
    await expect(
      page.getByRole("heading", {
        name: destination.heading,
        exact: true,
      }),
    ).toBeVisible();
    const refreshed = await page.reload();
    expect(refreshed?.status()).toBe(200);
    await expect(
      page.getByRole("heading", {
        name: destination.heading,
        exact: true,
      }),
    ).toBeVisible();
    await expect(page).toHaveURL(destination.path);
    await expect(page.locator("iframe")).toHaveCount(0);
    expect(new URL(page.url()).hash).toBe("");
  }
});

test("browser back and forward restore pages and query parameters without replacing the shell", async ({
  page,
}) => {
  await mockApi(page);
  await page.goto("/exceptions?taskId=1046&type=local");
  await expect(page.getByRole("spinbutton", { name: "任务 ID" })).toHaveValue(
    "1046",
  );
  const toolbar = await page
    .getByRole("button", { name: "账号设置" })
    .elementHandle();
  await page.getByRole("link", { name: "存储连接", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "存储连接", exact: true }),
  ).toBeVisible();
  await page.getByRole("link", { name: "创建迁移", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "创建迁移", exact: true }),
  ).toBeVisible();
  await page.goBack();
  await expect(page).toHaveURL(/\/storage$/);
  await expect(
    page.getByRole("heading", { name: "存储连接", exact: true }),
  ).toBeVisible();
  await page.goBack();
  await expect(page).toHaveURL(/\/exceptions\?taskId=1046&type=local$/);
  await expect(page.getByRole("spinbutton", { name: "任务 ID" })).toHaveValue(
    "1046",
  );
  await expect(page.getByRole("tab", { name: "本地迁移失败" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  await page.goForward();
  await expect(page).toHaveURL(/\/storage$/);
  await expect(
    page.getByRole("heading", { name: "存储连接", exact: true }),
  ).toBeVisible();
  await page.goForward();
  await expect(page).toHaveURL(/\/create$/);
  await expect(
    page.getByRole("heading", { name: "创建迁移", exact: true }),
  ).toBeVisible();
  expect(await toolbar!.evaluate((element) => element.isConnected)).toBe(true);
});

test("theme survives page changes and logout remains effective after opening a deep link", async ({
  page,
}) => {
  await mockApi(page);
  await page.goto("/tasks");
  await expect(
    page.getByRole("heading", { name: "迁移任务", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "切换主题" }).click();
  await expect(page.locator("html")).toHaveCSS("color-scheme", "dark");
  await page.getByRole("link", { name: "存储连接", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "存储连接", exact: true }),
  ).toBeVisible();
  await expect(page.locator("html")).toHaveCSS("color-scheme", "dark");
  await page.evaluate(() =>
    sessionStorage.setItem("oss-transfer-create-draft", "pending-draft"),
  );
  await page.getByRole("button", { name: "账号设置" }).click();
  await page.getByRole("menuitem", { name: "退出登录" }).click();
  await expect(page.getByRole("heading", { name: "登录控制台" })).toBeVisible();
  expect(
    await page.evaluate(() =>
      sessionStorage.getItem("oss-transfer-create-draft"),
    ),
  ).toBeNull();
  await page.goto("/create");
  await expect(page.getByRole("heading", { name: "登录控制台" })).toBeVisible();
  await expect(page.locator("html")).toHaveCSS("color-scheme", "dark");
  expect(
    await page.evaluate(() => sessionStorage.getItem("oss-transfer-token")),
  ).toBeNull();
});
