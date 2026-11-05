import { expect, test, type Page } from "@playwright/test";
import { installPausedClock } from "./clock";

/** Opens the hook fixture on a paused clock with the given refresh interval. */
async function open(page: Page, refreshMs: number) {
  await installPausedClock(page);
  await page.goto(`/tests/stable-messages.html?refreshMs=${refreshMs}`);
  await expect
    .poll(() => page.evaluate(() => typeof window.pushSnapshot))
    .toBe("function");
}

const push = (page: Page, messages: string[]) =>
  page.evaluate((list) => window.pushSnapshot(list), messages);
const shown = (page: Page) => page.getByTestId("shown").getByRole("listitem");

test("a refresh interval no longer than the stable period needs two snapshots", async ({
  page,
}) => {
  await open(page, 1000);
  await push(page, ["problem"]);
  await page.clock.runFor(10_000);
  await expect(shown(page)).toHaveCount(0);
  await push(page, ["problem"]);
  await expect(shown(page)).toHaveText(["problem"]);
  // Hiding is symmetric: one clean snapshot, however long ago, is not enough.
  await push(page, []);
  await page.clock.runFor(10_000);
  await expect(shown(page)).toHaveText(["problem"]);
  await push(page, []);
  await expect(shown(page)).toHaveCount(0);
});

test("a slow refresh decides by time alone, so the delay stays bounded", async ({
  page,
}) => {
  await open(page, 30_000);
  await push(page, ["problem"]);
  await page.clock.runFor(3_000);
  await expect(shown(page)).toHaveCount(0);
  await page.clock.runFor(1_500);
  await expect(shown(page)).toHaveText(["problem"]);
  await push(page, []);
  await page.clock.runFor(4_500);
  await expect(shown(page)).toHaveCount(0);
});

test("a re-check timer that fires before the wall clock reaches the threshold re-arms", async ({
  page,
}) => {
  await open(page, 0);
  await push(page, ["problem"]);
  // The timer was armed for four seconds. The wall clock steps back two
  // seconds, so when it fires the threshold has not been reached yet.
  await page.clock.setSystemTime(
    (await page.evaluate(() => Date.now())) - 2_000,
  );
  await page.clock.runFor(4_500);
  await expect(shown(page)).toHaveCount(0);
  await page.clock.runFor(3_000);
  await expect(shown(page)).toHaveText(["problem"]);
});
