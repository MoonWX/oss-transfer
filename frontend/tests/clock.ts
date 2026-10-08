import { expect, type Locator, type Page } from "@playwright/test";

export async function installPausedClock(page: Page) {
  const baseline = new Date("2026-09-22T12:00:00Z");
  await page.clock.install({ time: baseline });
  // Freeze before application timers exist, without reading a running clock over RPC.
  await page.clock.pauseAt(new Date(baseline.getTime() + 60_000));
}

export async function advanceUntilVisible(page: Page, target: Locator) {
  await expect
    .poll(async () => {
      await page.clock.runFor(400);
      return target.isVisible();
    })
    .toBe(true);
}
