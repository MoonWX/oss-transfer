import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

const emptyNodes = { "10.0.1.12": [], "10.0.1.13": [] };
const emptyState = "暂无已删除任务";

test("successful empty responses from every worker show a normal empty recycle bin", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/deleted-tasks", (route) =>
    route.fulfill({
      json: {
        success: true,
        deletedTasks: emptyNodes,
        totalCount: 0,
        complete: true,
        partial: false,
        failedWorkers: [],
      },
    }),
  );
  await page.goto("/tools?tab=recycle");
  await expect(page.getByText(emptyState, { exact: true })).toBeVisible();
  await expect(page.locator(".MuiAlert-standardWarning")).toHaveCount(0);
  await expect(
    page.getByText(/未返回回收站数据|当前列表可能不完整/),
  ).toHaveCount(0);
});

for (const scenario of [
  {
    name: "missing worker key",
    data: { deletedTasks: { "10.0.1.12": [] }, complete: false, partial: true },
    warning: "以下节点未返回回收站数据",
    node: "10.0.1.13",
  },
  {
    name: "missing keys despite a conflicting complete flag",
    data: { deletedTasks: {}, complete: true, partial: false },
    warning: "以下节点未返回回收站数据",
    node: "10.0.1.12",
  },
  {
    name: "explicit failed worker outside the discovery result",
    data: {
      deletedTasks: emptyNodes,
      complete: false,
      partial: true,
      failedWorkers: ["10.0.1.99"],
    },
    warning: "以下节点未返回回收站数据",
    node: "10.0.1.99",
  },
  {
    name: "partial response without named workers",
    data: { deletedTasks: emptyNodes, complete: false, partial: true },
    warning: "部分回收站记录未返回",
    node: "",
  },
  {
    name: "incomplete response without a partial flag",
    data: { deletedTasks: emptyNodes, complete: false, partial: false },
    warning: "部分回收站记录未返回",
    node: "",
  },
]) {
  test(`${scenario.name} shows a warning without claiming the recycle bin is empty`, async ({
    page,
  }) => {
    await mockApi(page);
    await page.route("**/api/master/deleted-tasks", (route) =>
      route.fulfill({
        json: { success: true, totalCount: 0, ...scenario.data },
      }),
    );
    await page.goto("/tools?tab=recycle");
    const warning = page
      .getByRole("alert")
      .filter({ hasText: scenario.warning });
    await expect(warning).toBeVisible();
    if (scenario.node) await expect(warning).toContainText(scenario.node);
    await expect(page.getByText(emptyState, { exact: true })).toHaveCount(0);
    await expect(
      page.getByRole("button", { name: "永久删除", exact: true }),
    ).toBeDisabled();
  });
}

test("legacy empty recycle results wait for node membership before claiming completeness", async ({
  page,
}) => {
  await mockApi(page);
  let release!: () => void;
  const workerReady = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route("**/api/master/nodes", async (route) => {
    await workerReady;
    await route.fallback();
  });
  await page.route("**/api/master/deleted-tasks", (route) =>
    route.fulfill({
      json: {
        success: true,
        deletedTasks: emptyNodes,
        totalCount: 0,
        complete: true,
        partial: false,
      },
    }),
  );
  await page.goto("/tools?tab=recycle");
  await expect(
    page.getByText("0 条节点任务记录", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText(emptyState, { exact: true })).toHaveCount(0);
  release();
  await expect(page.getByText(emptyState, { exact: true })).toBeVisible();
});

test("legacy node discovery failure does not claim a complete empty recycle bin", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/nodes", (route) =>
    route.fulfill({
      status: 503,
      json: { success: false, message: "节点清单不可用" },
    }),
  );
  await page.route("**/api/master/deleted-tasks", (route) =>
    route.fulfill({
      json: {
        success: true,
        deletedTasks: emptyNodes,
        totalCount: 0,
        complete: true,
        partial: false,
      },
    }),
  );
  await page.goto("/tools?tab=recycle");
  await expect(
    page.getByText("节点清单读取失败，无法确认回收站是否覆盖全部节点。", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(page.getByText(emptyState, { exact: true })).toHaveCount(0);
});

test("an authoritative recycle snapshot does not depend on a separate node request", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/nodes", (route) =>
    route.fulfill({
      status: 503,
      json: { success: false, message: "节点清单不可用" },
    }),
  );
  await page.route("**/api/master/deleted-tasks", (route) =>
    route.fulfill({
      json: {
        success: true,
        deletedTasks: emptyNodes,
        totalCount: 0,
        complete: true,
        queriedWorkers: Object.keys(emptyNodes),
        failedWorkers: [],
      },
    }),
  );
  await page.goto("/tools?tab=recycle");
  await expect(page.getByText(emptyState, { exact: true })).toBeVisible();
  await expect(page.locator(".MuiAlert-standardWarning")).toHaveCount(0);
});

test("snapshot membership still detects an omitted node when live discovery is unavailable", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/nodes", (route) =>
    route.fulfill({
      status: 503,
      json: { success: false, message: "节点清单不可用" },
    }),
  );
  await page.route("**/api/master/deleted-tasks", (route) =>
    route.fulfill({
      json: {
        success: true,
        deletedTasks: emptyNodes,
        totalCount: 0,
        complete: true,
        queriedWorkers: [...Object.keys(emptyNodes), "10.0.1.99"],
        failedWorkers: [],
      },
    }),
  );
  await page.goto("/tools?tab=recycle");
  await expect(
    page.getByRole("alert").filter({ hasText: "以下节点未返回回收站数据" }),
  ).toContainText("10.0.1.99");
  await expect(page.getByText(emptyState, { exact: true })).toHaveCount(0);
});
