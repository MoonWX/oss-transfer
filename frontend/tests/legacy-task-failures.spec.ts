import { expect, test } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

const note =
  "该任务从 v4.0.x 转换而来，其失败对象不在异常记录中列出。请导出该任务的任务报告，在 file-results.csv 和 exception-details.csv 中查看。";

// A task converted from v4.0.x keeps its failed count, but its failed objects
// are imported history that only its task report contains.
const converted = {
  ...tasks[3],
  taskIndex: 224,
  status: "PARTIAL_FAILED",
  failedObjectCount: 37,
  legacyHistorical: true,
};

test("a converted v4.0.x task does not link its failed count to an empty list", async ({
  page,
}) => {
  await mockApi(page);
  const current = {
    ...tasks[0],
    status: "PARTIAL_FAILED",
    failedObjectCount: 5,
  };
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: 2, tasks: [current, converted] } }),
  );
  await page.goto("/tasks");
  const row = (id: number) =>
    page.getByRole("row").filter({
      has: page.getByRole("button", { name: `查看任务 ${id}` }),
    });
  await expect(
    row(current.taskIndex).getByRole("link", { name: "5", exact: true }),
  ).toHaveAttribute("href", `/exceptions?taskId=${current.taskIndex}&type=oss`);
  const count = row(224).getByTestId("legacy-failed-count");
  await expect(count).toHaveText("37");
  await expect(row(224).getByRole("link", { name: "37" })).toHaveCount(0);
  await count.hover();
  await expect(page.getByRole("tooltip")).toHaveText(note);

  await page.route("**/api/master/tasks/224", (route) =>
    route.fulfill({ json: { success: true, data: converted } }),
  );
  await page.goto("/tasks?taskId=224");
  const drawer = page.locator(".MuiDrawer-paper").filter({
    has: page.getByRole("heading", { name: "迁移任务 #224" }),
  });
  await expect(drawer.getByTestId("legacy-failures-note")).toHaveText(note);
  await expect(
    drawer.getByRole("link", { name: /查看失败对象|查看异常记录/ }),
  ).toHaveCount(0);
});

test("the exception records say where a converted task's failures are", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/failed", (route) =>
    route.fulfill({
      json: {
        success: true,
        failedKeys: { "10.0.1.12": [], "10.0.1.12Count": 0 },
        totalCount: 0,
        queriedWorkers: ["10.0.1.12"],
        failedWorkers: [],
        unreachableWorkers: [],
        complete: true,
        legacyHistorical: true,
      },
    }),
  );
  await page.route("**/api/master/fallback", (route) =>
    route.fulfill({
      json: {
        success: true,
        fallbackObjects: { "10.0.1.12": [], "10.0.1.12Count": 0 },
        totalCount: 0,
        queriedWorkers: ["10.0.1.12"],
        failedWorkers: [],
        unreachableWorkers: [],
        complete: true,
        supported: false,
      },
    }),
  );
  await page.goto("/exceptions?taskId=224&type=oss");
  const region = page.getByRole("region", { name: "失败对象" });
  await expect(region).toContainText(note);
  await expect(region).not.toContainText(
    "请调整筛选条件，或在任务执行后刷新。",
  );

  await page.getByRole("tab", { name: "降级迁移记录" }).click();
  await expect(
    page.getByText(
      "当前版本不会改用备用传输方式，因此不产生降级记录；从 v4.0.x 转换的任务的降级记录不在此列出。",
      { exact: true },
    ),
  ).toBeVisible();
  await expect(
    page.getByRole("region", { name: "降级迁移记录" }),
  ).not.toContainText("请调整筛选条件，或在任务执行后刷新。");
});
