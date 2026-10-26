import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

async function submit(page: Page) {
  return page.evaluate(async () => {
    const modulePath = "/src/lib/api.tsx";
    const { api } = await import(modulePath);
    return api("/api/test-version", {
      method: "POST",
      body: { operation: "once" },
    });
  });
}

test("a new server build prompts without replaying requests or replacing the open page", async ({
  page,
}) => {
  await mockApi(page);
  await page.goto("/tasks");
  await expect(
    page.getByRole("heading", { name: "迁移任务", exact: true }),
  ).toBeVisible();
  const clientCommit = (await page.locator("footer code").innerText()).trim();
  let serverCommit = clientCommit;
  let calls = 0;
  await page.route("**/api/test-version", async (route) => {
    calls++;
    expect(route.request().method()).toBe("POST");
    expect(route.request().postDataJSON()).toEqual({ operation: "once" });
    await route.fulfill({
      headers: { "X-OSS-Transfer-Commit": serverCommit },
      json: { success: true, accepted: calls },
    });
  });
  const notice = page
    .getByRole("alert")
    .filter({ hasText: "服务已更新，请刷新页面" });
  await submit(page);
  await expect(notice).toHaveCount(0);
  serverCommit = "unknown";
  await submit(page);
  await expect(notice).toHaveCount(0);

  await page.evaluate(() => Object.assign(window, { originalDocument: true }));
  serverCommit = clientCommit === "abcdef0" ? "abcdef1" : "abcdef0";
  expect(await submit(page)).toEqual({ success: true, accepted: 3 });
  await expect(notice).toBeVisible();
  expect(calls).toBe(3);
  expect(
    await page.evaluate(() => Reflect.get(window, "originalDocument")),
  ).toBe(true);

  // A delayed response from the previous process cannot dismiss the notice.
  serverCommit = clientCommit;
  await submit(page);
  await expect(notice).toBeVisible();
  await page.getByRole("link", { name: "异常记录", exact: true }).click();
  await expect(notice).toBeVisible();
  await page.waitForTimeout(1100);
  expect(calls).toBe(4);
  expect(
    await page.evaluate(() => Reflect.get(window, "originalDocument")),
  ).toBe(true);

  await notice.getByRole("button", { name: "刷新页面", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "异常记录", exact: true }),
  ).toBeVisible();
  await expect(notice).toHaveCount(0);
  expect(
    await page.evaluate(() => Reflect.get(window, "originalDocument")),
  ).toBeUndefined();
  expect(calls).toBe(4);
});

for (const [locale, message, button] of [
  ["en-US", "The server has been updated. Refresh this page.", "Refresh page"],
  [
    "ja-JP",
    "サーバーが更新されました。ページを再読み込みしてください。",
    "ページを再読み込み",
  ],
]) {
  test(`service update notice is translated in ${locale}`, async ({ page }) => {
    await mockApi(page);
    await page.addInitScript((language) => {
      localStorage.setItem("oss-transfer-language", language);
    }, locale);
    await page.route("**/api/test-version", (route) =>
      route.fulfill({
        headers: {
          "X-OSS-Transfer-Commit": "abcdef0123456789abcdef0123456789abcdef01",
        },
        json: { success: true },
      }),
    );
    await page.goto("/tasks");
    await expect(page.locator("footer code")).toBeVisible();
    await submit(page);
    const notice = page.getByRole("alert").filter({ hasText: message });
    await expect(notice).toBeVisible();
    await expect(
      notice.getByRole("button", { name: button, exact: true }),
    ).toBeVisible();
  });
}
