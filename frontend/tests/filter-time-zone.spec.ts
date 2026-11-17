import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

// The administrator's browser is in UTC+8; the Master (a Docker image without
// TZ) reads time filters in UTC, as the create form says.
test.use({ timezoneId: "Asia/Shanghai" });

async function showTask(page: Page, locale: string, filters: object) {
  await page.addInitScript((value) => {
    localStorage.setItem("oss-transfer-language", value);
  }, locale);
  await mockApi(page);
  const rows = tasks.map((task) =>
    task.taskIndex === 1048 ? { ...task, ...filters } : task,
  );
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: { success: true, totalTasks: rows.length, tasks: rows },
    }),
  );
  await page.goto("/tasks?taskId=1048");
}

const detailValue = (page: Page, section: string, label: string) =>
  page
    .getByText(section, { exact: true })
    .locator("..")
    .getByText(label, { exact: true })
    .locator("..");

for (const [locale, section, after, before, wantAfter, wantBefore] of [
  [
    "zh-CN",
    "执行信息",
    "修改时间晚于",
    "修改时间早于",
    "2026/11/1 00:00:00（UTC）",
    "2026/11/2 08:30:00（UTC+08:00）",
  ],
  [
    "en-US",
    "Execution details",
    "Modified after",
    "Modified before",
    "11/1/2026, 00:00:00 (UTC)",
    "11/2/2026, 08:30:00 (UTC+08:00)",
  ],
] as const) {
  test(`time filters are shown as entered, with the zone the Master read them in (${locale})`, async ({
    page,
  }) => {
    await showTask(page, locale, {
      dateAfter: "2026-11-01T00:00:00Z",
      dateBefore: "2026-11-02T08:30:00+08:00",
    });
    // Converted to the browser's zone, the first would read 08:00:00.
    await expect(detailValue(page, section, after)).toHaveText(
      after + wantAfter,
    );
    await expect(detailValue(page, section, before)).toHaveText(
      before + wantBefore,
    );
  });
}

test("a time filter without an offset keeps its wall-clock time", async ({
  page,
}) => {
  await showTask(page, "zh-CN", { dateAfter: "2026-11-01T00:00:00" });
  await expect(detailValue(page, "执行信息", "修改时间晚于")).toHaveText(
    "修改时间晚于2026/11/1 00:00:00",
  );
});
