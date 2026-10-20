import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

const initialPassword = "initial-password-for-browser-test";
const newPassword = "replacement-password-for-browser-test";

async function mockInitialLogin(page: Page, authenticated = false) {
  const requests = await mockApi(page, { authenticated });
  let mustChangePassword = true;
  let rejectChange = false;
  const changes: unknown[] = [];
  await page.route("**/api/auth/login", (route) =>
    route.fulfill({
      json: {
        success: true,
        token: "test-token",
        user: { username: "admin", mustChangePassword },
      },
    }),
  );
  await page.route("**/api/auth/current", (route) =>
    route.fulfill({
      json: {
        success: true,
        user: { username: "admin", mustChangePassword },
      },
    }),
  );
  await page.route("**/api/auth/change-password", async (route) => {
    changes.push(route.request().postDataJSON());
    if (rejectChange) {
      await route.fulfill({
        status: 400,
        json: { success: false, message: "密码过短" },
      });
      return;
    }
    mustChangePassword = false;
    await route.fulfill({
      json: {
        success: true,
        user: { username: "admin", mustChangePassword },
      },
    });
  });
  return {
    requests,
    changes,
    reject: (value: boolean) => {
      rejectChange = value;
    },
  };
}

async function login(page: Page) {
  await page.getByLabel(/^用户名(?:\s*\*)?$/).fill("admin");
  await page.getByLabel(/^密码(?:\s*\*)?$/).fill(initialPassword);
  await page.getByRole("button", { name: "登录", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "设置密码", exact: true }),
  ).toBeVisible();
}

async function fillNewPassword(page: Page) {
  await page.getByLabel(/^新密码(?:\s*\*)?$/).fill(newPassword);
  await page.getByLabel(/^确认新密码(?:\s*\*)?$/).fill(newPassword);
}

test("initial login stays outside the console until the password is changed", async ({
  page,
}) => {
  const state = await mockInitialLogin(page);
  await page.goto("/tasks");
  await login(page);
  await expect(page.getByRole("navigation")).toHaveCount(0);
  await expect(page.getByLabel(/^当前密码(?:\s*\*)?$/)).toHaveCount(0);
  expect(state.requests).toEqual([]);
  const stored = await page.evaluate(() => ({
    session: JSON.stringify(sessionStorage),
    local: JSON.stringify(localStorage),
  }));
  expect(JSON.stringify(stored)).not.toContain(initialPassword);
  await fillNewPassword(page);
  await page.getByRole("button", { name: "保存并继续", exact: true }).click();
  await expect(page.getByRole("heading", { name: "迁移任务" })).toBeVisible();
  expect(state.changes).toEqual([
    { oldPassword: initialPassword, newPassword },
  ]);
  expect(
    state.requests.some((request) => request.path.startsWith("/api/master/")),
  ).toBe(true);
  expect(
    state.requests.some((request) => request.path.startsWith("/api/setup/")),
  ).toBe(false);
});

test("refreshing a restricted session asks for the current password without loading business data", async ({
  page,
}) => {
  const state = await mockInitialLogin(page, true);
  await page.goto("/nodes");
  await expect(
    page.getByRole("heading", { name: "设置密码", exact: true }),
  ).toBeVisible();
  await page.reload();
  await expect(page.getByLabel(/^当前密码(?:\s*\*)?$/)).toBeVisible();
  await expect(page.getByRole("navigation")).toHaveCount(0);
  expect(state.requests).toEqual([]);
  await page.getByLabel(/^当前密码(?:\s*\*)?$/).fill(initialPassword);
  await fillNewPassword(page);
  await page.getByRole("button", { name: "保存并继续", exact: true }).click();
  await expect(page.getByRole("heading", { name: "执行节点" })).toBeVisible();
  expect(state.changes).toEqual([
    { oldPassword: initialPassword, newPassword },
  ]);
});

test("password validation failures keep the restricted session and allow retry", async ({
  page,
}) => {
  const state = await mockInitialLogin(page);
  await page.goto("/");
  await login(page);
  await fillNewPassword(page);
  await page.getByLabel(/^确认新密码(?:\s*\*)?$/).fill("different");
  await page.getByRole("button", { name: "保存并继续", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("两次输入的新密码不一致");
  expect(state.changes).toHaveLength(0);
  state.reject(true);
  await fillNewPassword(page);
  await page.getByRole("button", { name: "保存并继续", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("密码过短");
  expect(state.requests).toEqual([]);
  await expect(page.getByRole("navigation")).toHaveCount(0);
  state.reject(false);
  await page.getByRole("button", { name: "保存并继续", exact: true }).click();
  await expect(page.getByRole("navigation")).toBeVisible();
  expect(state.changes).toHaveLength(2);
});

test("the restricted user can sign out without entering the console", async ({
  page,
}) => {
  const state = await mockInitialLogin(page);
  await page.goto("/");
  await login(page);
  await page.getByRole("button", { name: "退出登录", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "登录控制台", exact: true }),
  ).toBeVisible();
  expect(
    await page.evaluate(() => sessionStorage.getItem("oss-transfer-token")),
  ).toBeNull();
  expect(
    state.requests.some((request) => request.path.startsWith("/api/master/")),
  ).toBe(false);
});
