import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

test("desktop sidebar controls live inside the sidebar and its rail animates with the workspace without moving icons", async ({
  page,
}, testInfo) => {
  await mockApi(page);
  await page.emulateMedia({ reducedMotion: "no-preference" });
  await page.goto("/tasks");
  const nav = page.getByRole("navigation", { name: "主导航" });
  const toggle = nav.getByRole("button", { name: "折叠侧栏", exact: true });
  await expect(toggle).toBeVisible();
  await expect(
    page.getByRole("banner").getByRole("button", { name: "折叠侧栏" }),
  ).toHaveCount(0);
  const link = nav.getByRole("link", { name: "迁移任务", exact: true });
  const icon = link.locator("svg");
  const iconBefore = (await icon.boundingBox())!;
  const linkHandle = await link.elementHandle();
  await toggle.focus();
  const frames = await toggle.evaluate((button) => {
    const nav = button.closest("nav")!;
    const paper = button.closest(".MuiDrawer-paper")!;
    const header = document.querySelector("header")!;
    (button as HTMLElement).click();
    return new Promise<{ width: number; paper: number; header: number }[]>(
      (resolve) => {
        const frames: { width: number; paper: number; header: number }[] = [];
        const start = performance.now();
        const sample = () => {
          frames.push({
            width: nav.getBoundingClientRect().width,
            paper: paper.getBoundingClientRect().width,
            header: header.getBoundingClientRect().left,
          });
          if (performance.now() - start >= 360) resolve(frames);
          else requestAnimationFrame(sample);
        };
        requestAnimationFrame(sample);
      },
    );
  });
  expect(frames.some((frame) => frame.width > 80 && frame.width < 248)).toBe(
    true,
  );
  expect(frames.at(-1)!.width).toBe(80);
  expect(
    frames.every(
      (frame) =>
        Math.abs(frame.paper - frame.width) < 1 &&
        Math.abs(frame.header - frame.width) < 1,
    ),
  ).toBe(true);
  const iconAfter = (await icon.boundingBox())!;
  expect(iconAfter.x).toBe(iconBefore.x);
  expect(iconAfter.y).toBe(iconBefore.y);
  expect(await linkHandle!.evaluate((element) => element.isConnected)).toBe(
    true,
  );
  await expect(
    nav.getByRole("button", { name: "展开侧栏", exact: true }),
  ).toBeFocused();
  await nav.getByRole("button", { name: "展开侧栏", exact: true }).blur();
  await page.mouse.move(600, 50);
  await expect(page.getByRole("tooltip")).toHaveCount(0);
  await page.screenshot({
    path: testInfo.outputPath("sidebar-rail-light.png"),
    animations: "disabled",
  });
  await nav
    .getByRole("button", { name: "展开侧栏", exact: true })
    .press("Enter");
  await expect.poll(async () => (await nav.boundingBox())!.width).toBe(248);
  await nav.getByRole("button", { name: "折叠侧栏", exact: true }).blur();
  await expect(page.getByRole("tooltip")).toHaveCount(0);
  expect(
    (await nav
      .getByRole("link", { name: "创建迁移", exact: true })
      .boundingBox())!.x,
  ).toBe(12);
  expect((await link.boundingBox())!.x).toBe(8);
  await page.screenshot({
    path: testInfo.outputPath("sidebar-expanded-light.png"),
    animations: "disabled",
  });
});

test("resize handle is visible at rest and follows pointer without animation or repeated storage writes", async ({
  page,
}, testInfo) => {
  await mockApi(page);
  await page.addInitScript(() => {
    const original = Storage.prototype.setItem;
    Object.assign(window, { sidebarWrites: 0 });
    Storage.prototype.setItem = function (key, value) {
      if (key === "oss-transfer-sidebar")
        Reflect.set(
          window,
          "sidebarWrites",
          Number(Reflect.get(window, "sidebarWrites")) + 1,
        );
      return original.call(this, key, value);
    };
  });
  await page.goto("/tasks");
  const nav = page.getByRole("navigation", { name: "主导航" });
  const handle = nav.getByRole("separator", { name: "调整侧栏宽度" });
  await expect(handle).toBeVisible();
  expect(
    await handle.evaluate(
      (element) => getComputedStyle(element, "::before").width,
    ),
  ).toBe("1px");
  const grip = await handle.evaluate((element) => {
    const style = getComputedStyle(element, "::after");
    return {
      width: parseFloat(style.width),
      height: parseFloat(style.height),
      opacity: parseFloat(style.opacity),
      color: style.backgroundColor,
    };
  });
  expect(grip.width).toBeGreaterThan(0);
  expect(grip.height).toBeGreaterThan(20);
  expect(grip.opacity).toBeGreaterThan(0);
  expect(grip.color).not.toBe("rgba(0, 0, 0, 0)");
  const before = await page.evaluate(() =>
    Reflect.get(window, "sidebarWrites"),
  );
  const box = (await handle.boundingBox())!;
  const x = box.x + box.width / 2;
  await page.mouse.move(x, 240);
  await page.mouse.down();
  for (const offset of [25, 60, 95]) {
    await page.mouse.move(x + offset, 240);
    await expect(handle).toHaveAttribute("aria-valuenow", String(248 + offset));
    expect((await nav.boundingBox())!.width).toBe(248 + offset);
    await expect(nav).toHaveCSS("transition-property", "none");
    expect(
      await page.evaluate(() => Reflect.get(window, "sidebarWrites")),
    ).toBe(before);
  }
  await page.mouse.up();
  await expect
    .poll(() => page.evaluate(() => Reflect.get(window, "sidebarWrites")))
    .toBe(before + 1);
  await expect(handle).toBeFocused();
  await handle.press("ArrowLeft");
  await expect(handle).toHaveCSS("outline-style", "solid");
  await expect(handle).toHaveAttribute("aria-valuenow", "335");
  await page.reload();
  await expect(handle).toHaveAttribute("aria-valuenow", "335");
  await page.getByRole("button", { name: "切换主题" }).click();
  await expect(page.locator("html")).toHaveCSS("color-scheme", "dark");
  await handle.hover();
  await page.screenshot({
    path: testInfo.outputPath("sidebar-expanded-dark.png"),
  });
});

test("reduced motion changes width immediately and mobile retains its own full drawer and header menu", async ({
  page,
}, testInfo) => {
  await mockApi(page);
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.goto("/tasks");
  const nav = page.getByRole("navigation", { name: "主导航" });
  await nav.getByRole("button", { name: "折叠侧栏", exact: true }).click();
  expect((await nav.boundingBox())!.width).toBe(80);
  await expect(nav).toHaveCSS("transition-property", "none");
  await nav.getByRole("button", { name: "展开侧栏", exact: true }).click();
  expect((await nav.boundingBox())!.width).toBe(248);
  await page.setViewportSize({ width: 390, height: 844 });
  const menu = page
    .getByRole("banner")
    .getByRole("button", { name: "打开导航", exact: true });
  await menu.click();
  await expect(
    page.getByRole("button", { name: "折叠侧栏", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("separator", { name: "调整侧栏宽度" }),
  ).toHaveCount(0);
  const link = page.getByRole("link", { name: "存储连接", exact: true });
  const paper = page.locator(".MuiDrawer-paper").filter({ has: link });
  await expect(paper).toHaveCSS("width", "264px");
  await page.screenshot({ path: testInfo.outputPath("sidebar-mobile.png") });
  await page.keyboard.press("Escape");
  await expect(menu).toBeFocused();
  await expect(paper).toHaveCount(0);
  await page.setViewportSize({ width: 1280, height: 720 });
  expect((await nav.boundingBox())!.width).toBe(248);
  await expect(
    nav.getByRole("button", { name: "折叠侧栏", exact: true }),
  ).toBeVisible();
});
