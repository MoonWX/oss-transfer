import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

// Pretends to be another operating system before the console starts.
async function emulatePlatform(page: Page, platform: "Windows" | "macOS") {
  await page.addInitScript((name) => {
    Object.defineProperty(navigator, "platform", {
      get: () => (name === "Windows" ? "Win32" : "MacIntel"),
    });
    Object.defineProperty(navigator, "userAgentData", {
      get: () => ({ platform: name }),
    });
  }, platform);
}

const bodyFont = (page: Page) =>
  page.evaluate(() => getComputedStyle(document.body).fontFamily);

const loadedFamilies = (page: Page) =>
  page.evaluate(() =>
    [...document.fonts]
      .filter((face) => face.status === "loaded")
      .map((face) => face.family.replaceAll('"', "")),
  );

test("Windows shows the bundled Google Sans and Noto Sans SC instead of system fonts", async ({
  page,
}) => {
  await emulatePlatform(page, "Windows");
  await mockApi(page);
  await page.goto("/tasks");
  await expect(page.locator("footer code")).toBeVisible();
  expect(await bodyFont(page)).toBe(
    '"Google Sans Variable", "Noto Sans SC Variable", sans-serif',
  );
  // The page's own English and Chinese text pulls in both bundled fonts.
  await expect
    .poll(() => loadedFamilies(page))
    .toEqual(
      expect.arrayContaining(["Google Sans Variable", "Noto Sans SC Variable"]),
    );
  const arrow = await page.evaluate(
    async () =>
      (await document.fonts.load('16px "Google Sans Variable"', "→")).length,
  );
  expect(arrow).toBe(1);
  // Monospaced text (the build commit in the footer) uses Google Sans Code.
  const code = page.locator("footer code");
  await expect(code).toHaveCSS(
    "font-family",
    '"Google Sans Code Variable", "Noto Sans SC Variable", monospace',
  );
  await expect
    .poll(() => loadedFamilies(page))
    .toEqual(expect.arrayContaining(["Google Sans Code Variable"]));
});

test("macOS shows the system font and downloads no bundled font", async ({
  page,
}) => {
  await emulatePlatform(page, "macOS");
  await mockApi(page);
  await page.goto("/tasks");
  await expect(page.locator("footer code")).toBeVisible();
  expect(await bodyFont(page)).toBe(
    '-apple-system, system-ui, "PingFang SC", sans-serif',
  );
  await expect(page.locator("footer code")).toHaveCSS(
    "font-family",
    'ui-monospace, SFMono-Regular, Menlo, "PingFang SC", monospace',
  );
  await page.evaluate(() => document.fonts.ready);
  expect(await loadedFamilies(page)).toEqual([]);
});

test("Japanese keeps the Japanese system fonts on Windows", async ({
  page,
}) => {
  await emulatePlatform(page, "Windows");
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-language", "ja-JP"),
  );
  await page.goto("/tasks");
  await expect(page.locator("html")).toHaveAttribute("lang", "ja-JP");
  await expect(page.locator("footer code")).toBeVisible();
  const family = await bodyFont(page);
  expect(family.startsWith('"Google Sans Variable", "Yu Gothic UI"')).toBe(
    true,
  );
  expect(family).not.toContain("Noto Sans SC");
});
