import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

// The English and Japanese tables are separate chunks. A missing chunk must not
// leave the page claiming a language whose text it cannot show.
const catalog = (language: string) => (url: URL) =>
  url.pathname.includes(`virtual:locale-catalog/${language}`) ||
  new RegExp(`/assets/${language}-[\\w-]+\\.js$`).test(url.pathname);

test.describe("an unavailable table at startup", () => {
  test.use({ locale: "en-US" });
  test("keeps the page and request language consistent in Chinese", async ({
    page,
  }) => {
    const languages: (string | undefined)[] = [];
    page.on("request", (request) => {
      if (new URL(request.url()).pathname.startsWith("/api/"))
        languages.push(request.headers()["accept-language"]);
    });
    await mockApi(page);
    await page.route(catalog("en"), (route) => route.fulfill({ status: 404 }));
    await page.goto("/create");
    await expect(
      page.getByRole("heading", { name: "创建迁移", exact: true }),
    ).toBeVisible();
    await expect(page.locator("html")).toHaveAttribute("lang", "zh-CN");
    expect(languages.length).toBeGreaterThan(0);
    expect(languages.every((value) => value === "zh-CN")).toBe(true);
  });
});

test.describe("an unavailable table on switching", () => {
  test.use({ locale: "zh-CN" });
  test("reloads into the saved language instead of mixing languages", async ({
    page,
  }) => {
    await mockApi(page);
    // As for a tab opened before an upgrade: the first request finds no
    // table, the reloaded page gets the current one.
    let failures = 0;
    await page.route(catalog("ja"), (route) =>
      failures++ === 0 ? route.fulfill({ status: 404 }) : route.fallback(),
    );
    await page.goto("/create");
    await expect(
      page.getByRole("heading", { name: "创建迁移", exact: true }),
    ).toBeVisible();
    const reloaded = page.waitForEvent("load");
    await page.getByRole("button", { name: "语言" }).click();
    await page.getByRole("menuitem", { name: "日本語", exact: true }).click();
    await reloaded;
    await expect(page.locator("html")).toHaveAttribute("lang", "ja-JP");
    await expect(
      page.getByRole("heading", { name: "転送を作成", exact: true }),
    ).toBeVisible();
    expect(failures).toBe(2);
  });
});
