import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

async function mockTaskCatalog(page: Page, count: number) {
  await page.route("**/api/master/exceptions/tasks?**", (route) =>
    route.fulfill({
      json: {
        data: {
          items: Array.from({ length: count }, (_, index) => ({
            taskIndex: index + 41,
            sourceEndpoint: { localPath: "/mnt/shared/training-datasets" },
            destinationEndpoint: { localPath: "/mnt/backups/corpus" },
          })),
          hasMore: false,
        },
      },
    }),
  );
}

test("the task list fits in a short laptop window", async ({ page }) => {
  await page.setViewportSize({ width: 1366, height: 643 });
  await page.emulateMedia({ reducedMotion: "reduce" });
  await mockApi(page);
  await mockTaskCatalog(page, 8);
  await page.goto("/exceptions?type=local");
  const input = page.getByRole("combobox", { name: "所属任务", exact: true });
  const field = (await input
    .locator("xpath=ancestor::div[contains(@class,'MuiInputBase-root')][1]")
    .boundingBox())!;
  await input.click();
  await expect(page.getByRole("option", { name: /^任务 #48 / })).toBeAttached();
  const list = (await page.locator(".MuiAutocomplete-paper").boundingBox())!;
  expect(list.y).toBeGreaterThanOrEqual(0);
  expect(list.y + list.height).toBeLessThanOrEqual(643);
  expect(
    list.y >= field.y + field.height || list.y + list.height <= field.y,
  ).toBe(true);
});

// WCAG contrast of the selected option's caption on its tinted background,
// composited over the list surface.
const selectedCaptionContrast = (page: Page) =>
  page.evaluate(async () => {
    // Let the option's background transition finish before sampling it.
    for (let frame = 0; frame < 2; frame++)
      await new Promise(requestAnimationFrame);
    const option = document.querySelector<HTMLElement>(
      '.MuiAutocomplete-option[aria-selected="true"]',
    )!;
    const caption = option.querySelector<HTMLElement>(
      ".MuiTypography-caption",
    )!;
    const surface = option.closest<HTMLElement>(".MuiAutocomplete-paper")!;
    const rgba = (color: string) => {
      const [r, g, b, a = 1] = color.match(/[\d.]+/g)!.map(Number);
      return { r, g, b, a };
    };
    const base = rgba(getComputedStyle(surface).backgroundColor);
    const tint = rgba(getComputedStyle(option).backgroundColor);
    const background = [
      tint.r * tint.a + base.r * (1 - tint.a),
      tint.g * tint.a + base.g * (1 - tint.a),
      tint.b * tint.a + base.b * (1 - tint.a),
    ];
    const text = rgba(getComputedStyle(caption).color);
    const luminance = ([r, g, b]: number[]) => {
      const channel = (value: number) => {
        const v = value / 255;
        return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
      };
      return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b);
    };
    const [light, dark] = [
      luminance(background),
      luminance([text.r, text.g, text.b]),
    ].sort((a, b) => b - a);
    return (light + 0.05) / (dark + 0.05);
  });

for (const mode of ["light", "dark"] as const) {
  test(`the selected task's caption stays readable in ${mode} mode`, async ({
    page,
  }) => {
    await page.emulateMedia({ reducedMotion: "reduce" });
    await mockApi(page);
    await page.addInitScript((value) => {
      localStorage.setItem("oss-transfer-theme", value);
    }, mode);
    await mockTaskCatalog(page, 4);
    await page.goto("/exceptions?type=local&taskId=42");
    const input = page.getByRole("combobox", { name: "所属任务", exact: true });
    await expect(input).toHaveValue("42");
    await input.click();
    const selected = page.getByRole("option", { name: /^任务 #42 / });
    await expect(selected).toHaveAttribute("aria-selected", "true");
    // Opened with the selected task highlighted.
    expect(await selectedCaptionContrast(page)).toBeGreaterThanOrEqual(4.5);
    // Selected but not highlighted, then highlighted again from the keyboard.
    await page.keyboard.press("ArrowDown");
    await expect(selected).not.toHaveClass(/Mui-focused/);
    expect(await selectedCaptionContrast(page)).toBeGreaterThanOrEqual(4.5);
    await page.keyboard.press("ArrowUp");
    await expect(selected).toHaveClass(/Mui-focused/);
    expect(await selectedCaptionContrast(page)).toBeGreaterThanOrEqual(4.5);
  });
}
