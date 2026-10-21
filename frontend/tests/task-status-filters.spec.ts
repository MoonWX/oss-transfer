import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

async function select(page: Page, field: string, value: string) {
  await page.getByRole("combobox", { name: field }).click();
  await page.getByRole("option", { name: value, exact: true }).click();
}

test("parent lifecycle filters keep scanning within running and omit child-only states", async ({
  page,
}) => {
  await mockApi(page);
  const states = [
    "RUNNING",
    "STOPPING",
    "RESUMING",
    "COMPLETED",
    "PARTIAL_FAILED",
    "FAILED",
    "STOPPED",
  ];
  const rows = states.map((status, index) => ({
    ...tasks[0],
    taskIndex: 200 + index,
    status,
  }));
  rows.push({
    ...rows[0],
    taskIndex: 210,
    ossTaskGroup: true,
    creationStatus: "RUNNING",
    scanComplete: false,
  } as (typeof rows)[number]);
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { tasks: rows } }),
  );
  await page.goto("/tasks");
  await page.getByRole("combobox", { name: "任务状态", exact: true }).click();
  await expect(page.getByRole("option")).toHaveText([
    "全部状态",
    "执行中",
    "停止中",
    "恢复中",
    "已完成",
    "部分失败",
    "失败",
    "已停止",
  ]);
  await page.getByRole("option", { name: "执行中", exact: true }).click();
  const table = page.getByRole("table", { name: "迁移任务列表" });
  await expect(table.getByRole("row")).toHaveCount(3);
  await expect(
    table.getByRole("button", { name: "#210", exact: true }),
  ).toBeVisible();
  await expect(table.locator(".MuiChip-root")).toHaveText(["执行中", "执行中"]);
  await expect(page.getByText("0 个待执行", { exact: true })).toHaveCount(0);
  await expect(
    page.getByText("含运行中和扫描完成的任务", { exact: true }),
  ).toHaveCount(0);
});

test("legacy status aliases share one filter and retained nodes filter by exact address", async ({
  page,
}) => {
  await mockApi(page);
  const rows = [
    { ...tasks[0], taskIndex: 301, status: "PENDING", workerNode: "10.0.1.1" },
    { ...tasks[0], taskIndex: 302, status: "WAITING", workerNode: "10.0.1.10" },
    {
      ...tasks[0],
      taskIndex: 303,
      status: "LIST_DONE",
      workerNode: "removed-node",
    },
    { ...tasks[0], taskIndex: 304, status: "RUNNING", workerNode: "10.0.1.1" },
  ];
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { tasks: rows } }),
  );
  await page.route("**/api/master/nodes", (route) =>
    route.fulfill({
      json: {
        data: {
          nodes: [
            { address: "10.0.1.1", label: "主机一", online: false },
            { address: "10.0.1.10", label: "主机十", online: true },
            { address: "A".repeat(44), label: "远端节点", online: false },
            { address: "10.0.2.1", label: "同名节点", online: true },
            { address: "10.0.2.2", label: "同名节点", online: false },
          ],
        },
      },
    }),
  );
  await page.goto("/tasks");
  const table = page.getByRole("table", { name: "迁移任务列表" });
  await page.getByRole("combobox", { name: "任务状态", exact: true }).click();
  await expect(
    page.getByRole("option", { name: "排队中", exact: true }),
  ).toHaveCount(1);
  await expect(
    page.getByRole("option", { name: "扫描完成", exact: true }),
  ).toHaveCount(0);
  await page.getByRole("option", { name: "排队中", exact: true }).click();
  await expect(table.getByRole("row")).toHaveCount(3);
  await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
  await expect(
    page.getByRole("option", { name: "远端节点", exact: true }),
  ).toBeVisible();
  await expect(page.getByRole("option", { name: /A{44}/ })).toHaveCount(0);
  await expect(
    page.getByRole("option", { name: "同名节点 (10.0.2.1)", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("option", { name: "同名节点 (10.0.2.2)", exact: true }),
  ).toBeVisible();
  await page.getByRole("option", { name: "主机一", exact: true }).click();
  await expect(table.getByRole("row")).toHaveCount(2);
  await expect(
    table.getByRole("button", { name: "#301", exact: true }),
  ).toBeVisible();
  await select(page, "任务状态", "执行中");
  await select(page, "执行节点", "全部节点");
  await expect(table.getByRole("row")).toHaveCount(3);
  await select(page, "执行节点", "removed-node");
  await expect(table.getByRole("row")).toHaveCount(2);
  await expect(
    table.getByRole("button", { name: "#303", exact: true }),
  ).toBeVisible();
  await expect(table.locator(".MuiChip-root")).toHaveText(["执行中"]);
});
