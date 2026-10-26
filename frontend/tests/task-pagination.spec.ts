import { expect, test } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

const records = (count: number) =>
  Array.from({ length: count }, (_, index) => ({
    ...tasks[1],
    taskIndex: index + 1,
    generation: 1,
    sourceBucket: `source-${index + 1}`,
  }));

test.beforeEach(async ({ page }) => {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "0"),
  );
});

test("page links, jumps and saved page sizes keep the current task range accessible", async ({
  page,
}) => {
  const rows = records(137);
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { tasks: rows } }),
  );
  await page.goto("/tasks");
  const pagination = page.getByRole("navigation", { name: "任务分页" });
  await pagination
    .getByRole("button", { name: "第 3 页", exact: true })
    .click();
  await expect(pagination).toContainText("21–30 / 137");
  await pagination.getByRole("spinbutton", { name: "跳至页码" }).fill("11");
  await pagination.getByRole("button", { name: "跳转", exact: true }).click();
  await expect(pagination).toContainText("101–110 / 137");

  await pagination.getByRole("combobox", { name: "每页" }).click();
  await page.getByRole("option", { name: "25", exact: true }).click();
  await expect(pagination).toContainText("101–125 / 137");
  await expect(
    page.getByRole("button", { name: "#37", exact: true }),
  ).toBeVisible();
  await pagination
    .getByRole("button", { name: "最后一页", exact: true })
    .click();
  await expect(pagination).toContainText("126–137 / 137");
  await pagination.getByRole("spinbutton", { name: "跳至页码" }).fill("999");
  await pagination.getByRole("spinbutton", { name: "跳至页码" }).press("Enter");
  await expect(
    pagination.getByRole("spinbutton", { name: "跳至页码" }),
  ).toHaveValue("6");
  await pagination.getByRole("button", { name: "第一页", exact: true }).click();
  await expect(pagination).toContainText("1–25 / 137");
  await page.reload();
  await expect(pagination.getByRole("combobox", { name: "每页" })).toHaveText(
    "25",
  );
  await expect(pagination).toContainText("1–25 / 137");
});

test("selecting and deselecting another page preserves earlier selections and submits exactly those tasks", async ({
  page,
}) => {
  const rows = records(32);
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { tasks: rows } }),
  );
  const submitted: number[][] = [];
  await page.route("**/api/master/start", (route) => {
    submitted.push(route.request().postDataJSON().taskIds);
    return route.fulfill({ json: { success: true, accepted: 11, failed: 0 } });
  });
  await page.goto("/tasks");
  const selectPage = page.getByRole("checkbox", { name: "选择当前页任务" });
  await selectPage.check();
  await page.getByRole("button", { name: "第 3 页", exact: true }).click();
  await selectPage.check();
  await expect(
    page.getByText("已选 20 项（最多 128 项）", { exact: true }),
  ).toBeVisible();
  await selectPage.uncheck();
  await expect(
    page.getByText("已选 10 项（最多 128 项）", { exact: true }),
  ).toBeVisible();
  await page
    .getByRole("checkbox", { name: "选择任务 12", exact: true })
    .check();
  await page.getByRole("button", { name: "刷新任务", exact: true }).click();
  await expect(page.getByText("21–30 / 32", { exact: true })).toBeVisible();
  await expect(
    page.getByText("已选 11 项（最多 128 项）", { exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "启动", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("button", { name: "确认启动", exact: true }).click();
  await expect.poll(() => submitted.length).toBe(1);
  expect(submitted[0].sort((a, b) => a - b)).toEqual([
    12, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32,
  ]);
});

test("refresh clamps a vanished last page and filters clear selections without overflowing a narrow screen", async ({
  page,
}) => {
  let rows = records(37);
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { tasks: rows } }),
  );
  await page.goto("/tasks");
  const pagination = page.getByRole("navigation", { name: "任务分页" });
  await pagination
    .getByRole("button", { name: "最后一页", exact: true })
    .click();
  await page.getByRole("checkbox", { name: "选择任务 7", exact: true }).check();
  rows = records(13);
  await page.getByRole("button", { name: "刷新任务", exact: true }).click();
  await expect(pagination).toContainText("11–13 / 13");
  await expect(
    page.getByText("已选 1 项（最多 128 项）", { exact: true }),
  ).toBeVisible();

  await page.setViewportSize({ width: 390, height: 844 });
  await expect(
    pagination.getByRole("spinbutton", { name: "跳至页码" }),
  ).toBeVisible();
  expect(
    await pagination.evaluate(
      (element) => element.scrollWidth <= element.clientWidth,
    ),
  ).toBe(true);
  await page
    .getByRole("textbox", { name: "搜索任务", exact: true })
    .fill("source-7");
  await expect(pagination).toContainText("1–1 / 1");
  await expect(
    page.getByRole("button", { name: "取消选择", exact: true }),
  ).toHaveCount(0);
});
