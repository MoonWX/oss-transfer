import { expect, test, type Page } from "@playwright/test";

const fields = {
  select: {
    testId: "select-field",
    label: "下拉选择",
    list: ".MuiMenu-paper",
  },
  autocomplete: {
    testId: "autocomplete-field",
    label: "自动补全",
    list: ".MuiAutocomplete-paper",
  },
} as const;
type Kind = keyof typeof fields;

const fieldHeight = 48;

async function load(
  page: Page,
  {
    top,
    height = 600,
    options = 3,
    mode = "light",
    value = "",
  }: {
    top: number;
    height?: number;
    options?: number;
    mode?: string;
    value?: string;
  },
) {
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.setViewportSize({ width: 900, height });
  const query = new URLSearchParams({
    mode,
    top: String(top),
    options: String(options),
    value,
  });
  await page.goto(`/tests/popup-placement.html?${query}`);
  await expect(page.getByRole("combobox", { name: "下拉选择" })).toBeVisible();
  await page.evaluate(() => document.fonts.ready);
}

async function openList(page: Page, kind: Kind) {
  const { testId, label, list } = fields[kind];
  const control = page.getByTestId(testId);
  await page.getByRole("combobox", { name: label }).click();
  const popup = page.locator(list);
  await expect(popup).toBeVisible();
  await expect(popup).toHaveCSS("transform", "none");
  await expect(control.locator("label")).toHaveClass(/MuiInputLabel-shrink/);
  return {
    popup,
    field: (await control.locator(".MuiInputBase-root").boundingBox())!,
    label: (await control.locator("label").boundingBox())!,
    list: (await popup.boundingBox())!,
  };
}

for (const kind of ["select", "autocomplete"] as const) {
  test(`${kind} list opens below its field with a gap when there is room`, async ({
    page,
  }) => {
    await load(page, { top: 40 });
    const { field, list } = await openList(page, kind);
    expect(list.y).toBeGreaterThanOrEqual(field.y + field.height + 3);
  });

  test(`${kind} list near the viewport bottom opens above without covering the field label`, async ({
    page,
  }) => {
    await load(page, { top: 600 - fieldHeight - 60 });
    const { field, label, list } = await openList(page, kind);
    expect(label.y).toBeLessThan(field.y);
    expect(list.y + list.height).toBeLessThanOrEqual(label.y);
    expect(list.y).toBeGreaterThanOrEqual(0);
  });
}

test("a long select menu without room on either side scrolls instead of covering its field", async ({
  page,
}) => {
  await load(page, { top: 250, height: 500, options: 12 });
  const { popup, label, list } = await openList(page, "select");
  expect(list.y + list.height).toBeLessThanOrEqual(label.y);
  expect(list.y).toBeGreaterThanOrEqual(15);
  expect(
    await popup.evaluate(
      (element) => element.scrollHeight > element.clientHeight,
    ),
  ).toBe(true);
});

for (const [side, top] of [
  ["below", 300],
  ["above", 650],
] as const) {
  test(`the selected option stays in view when a height-limited menu opens ${side} its field`, async ({
    page,
  }) => {
    await load(page, { top, height: 900, options: 20, value: "选项 20" });
    const { popup, field, label, list } = await openList(page, "select");
    expect(
      await popup.evaluate(
        (element) => element.scrollHeight > element.clientHeight,
      ),
    ).toBe(true);
    const option = page.getByRole("option", { name: "选项 20", exact: true });
    await expect(option).toBeFocused();
    const box = (await option.boundingBox())!;
    expect(box.y).toBeGreaterThanOrEqual(list.y);
    expect(box.y + box.height).toBeLessThanOrEqual(list.y + list.height);
    if (side === "above")
      expect(list.y + list.height).toBeLessThanOrEqual(label.y);
    else expect(list.y).toBeGreaterThanOrEqual(field.y + field.height + 3);
  });
}

test("an open select menu stays on screen and off its field when the window shrinks", async ({
  page,
}) => {
  await load(page, { top: 40, height: 600, options: 20 });
  const { popup, field } = await openList(page, "select");
  await page.setViewportSize({ width: 900, height: 400 });
  await expect
    .poll(async () => {
      const box = (await popup.boundingBox())!;
      return box.y >= field.y + field.height && box.y + box.height <= 400;
    })
    .toBe(true);
});

for (const mode of ["light", "dark"] as const) {
  for (const kind of ["select", "autocomplete"] as const) {
    test(`${kind} list stands out from the card in ${mode} mode`, async ({
      page,
    }) => {
      await load(page, { top: 40, mode });
      const { popup } = await openList(page, kind);
      const surface = await popup.evaluate((element) => {
        const style = getComputedStyle(element);
        return {
          border: parseFloat(style.borderTopWidth),
          shadow: style.boxShadow,
          background: style.backgroundColor,
        };
      });
      const card = await page
        .getByTestId("card")
        .evaluate((element) => getComputedStyle(element).backgroundColor);
      expect(surface.border).toBeGreaterThanOrEqual(1);
      expect(surface.shadow).not.toBe("none");
      if (mode === "dark") expect(surface.background).not.toBe(card);
    });
  }
}
