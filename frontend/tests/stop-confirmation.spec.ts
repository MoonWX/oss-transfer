import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

const stopText =
  "停止扫描和传输。已完成的对象会保留，正在传输的对象在任务继续后从头传输。";

async function openStop(page: Page, task: Record<string, unknown>) {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "0"),
  );
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { tasks: [task] } }),
  );
  await page.route(`**/api/master/tasks/${task.taskIndex}`, (route) =>
    route.fulfill({ json: { success: true, data: task } }),
  );
  await page.goto(`/tasks?taskId=${task.taskIndex}`);
  await page.getByRole("button", { name: "停止任务", exact: true }).click();
  return page.getByRole("dialog", { name: "停止 1 个任务？", exact: true });
}

// A stop keeps completed objects but discards the bytes of objects in
// transfer; the confirmation must not promise that progress is kept.
test("the stop confirmation says objects in transfer start over and how much they sent", async ({
  page,
}) => {
  const dialog = await openStop(page, {
    ...tasks[0],
    status: "RUNNING",
    liveProgress: "LIVE",
    inFlightBytes: 3 * 2 ** 30,
    inFlightObjectCount: 12,
    inFlightObjectBytes: 3 * 2 ** 30,
  });
  await expect(dialog).toContainText(stopText);
  await expect(dialog.getByTestId("stop-in-flight")).toHaveText(
    "当前正在传输 12 个对象，已传输 3 GiB；任务继续后，这些对象需要从头重新传输。",
  );
  await expect(dialog).not.toContainText("保留已完成的对象与进度");
});

test("without a live sample the stop confirmation states the rule but no counts", async ({
  page,
}) => {
  const dialog = await openStop(page, {
    ...tasks[0],
    status: "RUNNING",
    liveProgress: "STALE",
    inFlightBytes: null,
  });
  await expect(dialog).toContainText(stopText);
  await expect(dialog.getByTestId("stop-in-flight")).toHaveCount(0);
});

for (const [locale, button, rule, count] of [
  [
    "en-US",
    "Stop task",
    "Completed objects are kept; objects in transfer start over from the beginning when the task continues.",
    "In transfer now: 1 object, 512 MiB sent so far.",
  ],
  [
    "ja-JP",
    "タスクを停止",
    "転送中のオブジェクトはタスクの再開後に最初から転送し直します。",
    "現在 1 個のオブジェクトを転送中で、512 MiB を転送済みです。",
  ],
]) {
  test(`the stop confirmation reads correctly in ${locale}`, async ({
    page,
  }) => {
    await page.addInitScript(
      (value) => localStorage.setItem("oss-transfer-language", value),
      locale,
    );
    await mockApi(page);
    await page.addInitScript(() =>
      localStorage.setItem("oss-transfer-refresh-seconds", "0"),
    );
    const task = {
      ...tasks[0],
      status: "RUNNING",
      liveProgress: "LIVE",
      inFlightObjectCount: 1,
      inFlightObjectBytes: 512 * 2 ** 20,
    };
    await page.route("**/api/master/status", (route) =>
      route.fulfill({ json: { tasks: [task] } }),
    );
    await page.route(`**/api/master/tasks/${task.taskIndex}`, (route) =>
      route.fulfill({ json: { success: true, data: task } }),
    );
    await page.goto(`/tasks?taskId=${task.taskIndex}`);
    await page.getByRole("button", { name: button, exact: true }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toContainText(rule);
    await expect(dialog).toContainText(count);
  });
}
