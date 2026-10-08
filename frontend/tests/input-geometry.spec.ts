import { expect, test, type Locator } from "@playwright/test";
import { writeFile } from "node:fs/promises";
import { mockApi, storageConfigs } from "./fixtures";

async function geometry(field: Locator) {
  return field.evaluate((element) => {
    const root = element.querySelector<HTMLElement>(".MuiOutlinedInput-root")!;
    const label = element.querySelector<HTMLLabelElement>("label")!;
    const input = root.querySelector<HTMLElement>(
      ".MuiSelect-select, input:not([aria-hidden]), textarea:not([aria-hidden])",
    )!;
    const r = root.getBoundingClientRect(),
      l = label.getBoundingClientRect(),
      i = input.getBoundingClientRect();
    const style = getComputedStyle(input);
    const top = parseFloat(style.paddingTop),
      bottom = parseFloat(style.paddingBottom);
    return {
      height: r.height,
      labelCenter: l.top + l.height / 2 - r.top,
      contentCenter: i.top + top + (i.height - top - bottom) / 2 - r.top,
      inputTop: i.top - r.top,
      inputPadding: top + bottom,
      labelInsideHorizontally: l.left >= r.left && l.right <= r.right,
      inputInsideHorizontally: i.left >= r.left && i.right <= r.right + 1,
      textareaHeight: i.height,
    };
  });
}

async function expectSingle(field: Locator, floating: boolean) {
  const g = await geometry(field);
  expect(g.height).toBeCloseTo(48, 0);
  expect(Math.abs(g.contentCenter - g.height / 2)).toBeLessThan(1);
  expect(g.labelInsideHorizontally).toBeTruthy();
  expect(g.inputInsideHorizontally).toBeTruthy();
  await expect
    .poll(async () =>
      Math.abs((await geometry(field)).labelCenter - (floating ? 0 : 24)),
    )
    .toBeLessThan(1);
}

for (const mode of ["light", "dark"] as const) {
  for (const viewport of [
    { width: 1200, height: 900 },
    { width: 360, height: 800 },
  ]) {
    test(`input geometry remains aligned in ${mode} at ${viewport.width}px`, async ({
      page,
    }, testInfo) => {
      await page.setViewportSize(viewport);
      await page.goto(`/tests/input-geometry.html?mode=${mode}`);
      await expect(
        page.getByRole("heading", { name: /输入控件/ }),
      ).toBeVisible();
      await page.evaluate(() => document.fonts.ready);
      const ids = [
        "select",
        "autocomplete",
        "text",
        "password",
        "number",
        "date",
      ];
      for (const id of ids)
        await expectSingle(page.getByTestId(id), id === "date");
      const empty = Object.fromEntries(
        await Promise.all(
          ids.map(async (id) => [id, await geometry(page.getByTestId(id))]),
        ),
      );
      const multiline = page.getByTestId("multiline");
      const initial = await geometry(multiline);
      expect(initial.inputTop).toBeCloseTo(12, 0);
      expect(initial.inputPadding).toBe(0);
      expect(initial.height).toBeGreaterThan(80);
      await page.screenshot({
        path: testInfo.outputPath("empty.png"),
        fullPage: true,
      });

      for (const id of ids) {
        const field = page.getByTestId(id);
        const input = field
          .locator(id === "select" ? '[role="combobox"]' : "input")
          .first();
        await input.focus();
        await expectSingle(field, true);
        await page.getByTestId("blur").click();
        await expectSingle(field, id === "date");
      }
      await multiline.locator("textarea").first().focus();
      await expect
        .poll(async () => Math.abs((await geometry(multiline)).labelCenter))
        .toBeLessThan(1);
      await page.getByTestId("blur").click();

      await page.getByTestId("select").getByRole("combobox").click();
      await page
        .getByRole("option", { name: "production", exact: true })
        .click();
      for (const [id, value] of Object.entries({
        autocomplete: "media-archive",
        text: "2026/图片归档/",
        password: "example-password",
        number: "1000",
        date: "2026-09-22T12:30",
      })) {
        await page.getByTestId(id).locator("input").first().fill(value);
        await page.getByTestId("blur").click();
      }
      await multiline
        .locator("textarea")
        .first()
        .fill(
          "source,bucket\nproduction,media\nbackup,archive\nline4\nline5\nline6",
        );
      await page.getByTestId("blur").click();
      for (const id of ids) await expectSingle(page.getByTestId(id), true);
      const expanded = await geometry(multiline);
      expect(expanded.height).toBeGreaterThan(initial.height + 50);
      expect(expanded.inputTop).toBeCloseTo(12, 0);
      expect(expanded.inputPadding).toBe(0);
      expect(expanded.inputInsideHorizontally).toBeTruthy();
      await expect
        .poll(async () => Math.abs((await geometry(multiline)).labelCenter))
        .toBeLessThan(1);
      expect(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= window.innerWidth,
        ),
      ).toBeTruthy();
      await page.screenshot({
        path: testInfo.outputPath("filled.png"),
        fullPage: true,
      });
      await page.getByTestId("text").locator("input").focus();
      await expectSingle(page.getByTestId("text"), true);
      await page.screenshot({
        path: testInfo.outputPath("focused.png"),
        fullPage: true,
      });
      const filled = Object.fromEntries(
        await Promise.all(
          ids.map(async (id) => [id, await geometry(page.getByTestId(id))]),
        ),
      );
      await writeFile(
        testInfo.outputPath("geometry.json"),
        JSON.stringify(
          {
            mode,
            viewport,
            empty,
            filled,
            multiline: { empty: initial, filled: expanded },
          },
          null,
          2,
        ),
      );
    });
  }
}

test("the actual create form keeps connection and bucket inputs at the same height", async ({
  page,
}, testInfo) => {
  await mockApi(page);
  await page.goto("/create");
  for (const name of ["源存储连接", "源存储桶"]) {
    const field = page
      .locator(".MuiFormControl-root")
      .filter({ has: page.getByRole("combobox", { name, exact: true }) });
    await expectSingle(field, false);
  }
  await page.screenshot({
    path: testInfo.outputPath("actual-create-empty.png"),
    fullPage: true,
  });
});

for (const width of [1200, 360]) {
  test(`selected connection endpoints stay below inputs and wrap at ${width}px`, async ({
    page,
  }, testInfo) => {
    await page.setViewportSize({ width, height: 900 });
    await mockApi(page);
    const configs = storageConfigs.map((config) => ({
      ...config,
      endpoint: `https://${config.alias}.${"private-storage-region-".repeat(7)}example.test`,
    }));
    await page.route("**/api/master/s3-config", async (route) => {
      await route.fulfill({ json: { success: true, configs } });
    });
    await page.goto("/create");
    await page.evaluate(() => document.fonts.ready);
    for (const [index, side] of ["源", "目标"].entries()) {
      await page
        .getByRole("combobox", { name: `${side}存储连接`, exact: true })
        .click();
      await page
        .getByRole("option", { name: configs[index].alias, exact: true })
        .click();
      await expect(page.locator(".MuiPopover-root")).toHaveCount(0);
    }
    await page.evaluate(() => window.scrollTo(0, 0));
    const measurements = [];
    for (const [index, side] of ["源", "目标"].entries()) {
      const connection = page.locator(".MuiFormControl-root").filter({
        has: page.getByRole("combobox", {
          name: new RegExp(`^${side}存储连接(?: |$)`),
        }),
      });
      await expectSingle(connection, true);
      const outline = await connection
        .locator(".MuiOutlinedInput-root")
        .boundingBox();
      const endpoint = page.getByText(configs[index].endpoint, { exact: true });
      await expect(endpoint).toBeVisible();
      const helper = await endpoint.boundingBox();
      const lineHeight = await endpoint.evaluate((element) =>
        parseFloat(getComputedStyle(element).lineHeight),
      );
      const nextField = await page
        .locator(".MuiFormControl-root")
        .filter({
          has: page.getByRole("combobox", {
            name: `${side}存储桶`,
            exact: true,
          }),
        })
        .locator(".MuiOutlinedInput-root")
        .boundingBox();
      measurements.push({ side, outline, helper, lineHeight, nextField });
    }
    const documentWidth = await page.evaluate(
      () => document.documentElement.scrollWidth,
    );
    await writeFile(
      testInfo.outputPath("connection-endpoints.json"),
      JSON.stringify({ width, documentWidth, measurements }, null, 2),
    );
    await page.screenshot({
      path: testInfo.outputPath("connection-endpoints.png"),
      fullPage: true,
    });
    for (const {
      side,
      outline,
      helper,
      lineHeight,
      nextField,
    } of measurements) {
      expect
        .soft(helper!.y, `${side} endpoint must be below the outline`)
        .toBeGreaterThanOrEqual(outline!.y + outline!.height + 1);
      expect
        .soft(nextField!.y, `${side} bucket must clear the entire endpoint`)
        .toBeGreaterThanOrEqual(helper!.y + helper!.height + 8);
      expect
        .soft(helper!.height, `${side} long endpoint must wrap`)
        .toBeGreaterThan(lineHeight * 1.5);
      expect
        .soft(helper!.x, `${side} endpoint stays within the input width`)
        .toBeGreaterThanOrEqual(outline!.x);
      expect
        .soft(helper!.x + helper!.width)
        .toBeLessThanOrEqual(outline!.x + outline!.width + 1);
    }
    expect(documentWidth).toBeLessThanOrEqual(width);
  });
}
