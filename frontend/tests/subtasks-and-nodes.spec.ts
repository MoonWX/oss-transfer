import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";
import { numberSubtasks } from "../src/lib/subtasks";

async function configureOss(page: Page) {
  await page.goto("/create");
  await page.getByRole("combobox", { name: "源存储连接" }).click();
  await page.getByRole("option", { name: "production", exact: true }).click();
  await page.getByRole("combobox", { name: "源存储桶" }).fill("media-source");
  await page.getByRole("combobox", { name: "目标存储连接" }).click();
  await page.getByRole("option", { name: "backup", exact: true }).click();
  await page.getByRole("combobox", { name: "目标存储桶" }).fill("media-target");
  await page.getByRole("button", { name: /迁移选项/ }).click();
}

test("legacy subtask numbers remain parent-local and stable across response order", () => {
  const rows = [
    { taskIndex: -16666501, shardIndex: 1 },
    { taskIndex: -16666507, shardIndex: 0 },
    { taskIndex: -16666505, shardIndex: 2 },
  ];
  expect(
    numberSubtasks(rows).map((row) => [row.taskIndex, row.subtaskNumber]),
  ).toEqual([
    [-16666507, 1],
    [-16666501, 2],
    [-16666505, 3],
  ]);
  expect(numberSubtasks([...rows].reverse())).toEqual(numberSubtasks(rows));
  expect(numberSubtasks([rows[2]])[0].subtaskNumber).toBe(3);
  expect(
    numberSubtasks([{ taskIndex: 921 }, { taskIndex: 918 }], [918, 921]).map(
      (row) => row.subtaskNumber,
    ),
  ).toEqual([1, 2]);
});

for (const direction of ["OSS_TO_OSS", "OSS_TO_LOCAL", "LOCAL_TO_OSS"]) {
  test(`${direction} displays parent-prefixed subtask numbering independently per migration`, async ({
    page,
  }) => {
    await mockApi(page);
    const parents = [222, 223].map((id) => ({
      ...tasks[1],
      direction,
      taskIndex: id,
      status: "COMPLETED",
      ossTaskGroup: direction === "OSS_TO_OSS",
      creationStatus: "COMPLETED",
      scanComplete: true,
      shards: [1, 0].map((index) => ({
        taskIndex:
          direction === "OSS_TO_OSS" ? id + index + 500 : -1670275759 - index,
        shardIndex: index,
        parentTaskId: id,
        status: "COMPLETED",
        workerNode: "10.0.1.12",
      })),
    }));
    await page.route("**/api/master/status", (route) =>
      route.fulfill({ json: { totalTasks: 2, tasks: parents } }),
    );
    await page.route("**/api/master/local-transfers/status?*", (route) => {
      const id = Number(
        new URL(route.request().url()).searchParams.get("taskId"),
      );
      const parent = parents.find((row) => row.taskIndex === id)!;
      return route.fulfill({
        json: {
          success: true,
          data: { tasks: [parent], shards: parent.shards },
        },
      });
    });
    for (const id of [222, 223]) {
      await page.goto(`/tasks?taskId=${id}`);
      const drawer = page.locator(".MuiDrawer-paper").filter({
        has: page.getByRole("heading", {
          name: `迁移任务 #${id}`,
          exact: true,
        }),
      });
      await expect(
        drawer.getByRole("heading", { name: "子任务 (2)", exact: true }),
      ).toBeVisible();
      await expect(drawer.getByText(/^子任务 #/)).toHaveText([
        `子任务 #${id}-1 · 10.0.1.12`,
        `子任务 #${id}-2 · 10.0.1.12`,
      ]);
      await expect(drawer.getByText(/子任务 #-|分片/)).toHaveCount(0);
    }
  });
}

test("OSS creation defaults to every reachable node and excludes unavailable nodes", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.route("**/api/master/local-nodes", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          nodes: [
            {
              executionNodeIp: "10.0.1.12",
              reachable: true,
              localTransferSupported: false,
            },
            {
              executionNodeIp: "10.0.1.13",
              reachable: true,
              localTransferSupported: true,
            },
            {
              executionNodeIp: "10.0.1.14",
              reachable: false,
              localTransferSupported: true,
            },
          ],
        },
      },
    }),
  );
  await configureOss(page);
  await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
  await expect(
    page.getByRole("option", { name: "10.0.1.14 · 不可用" }),
  ).toBeDisabled();
  await page.keyboard.press("Escape");
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
  await expect
    .poll(
      () => requests.filter((r) => r.path === "/api/master/add-buckets").length,
    )
    .toBe(1);
  expect(
    requests.find((r) => r.path === "/api/master/add-buckets")!.body[0]
      .eligibleWorkerNodes,
  ).toEqual(["10.0.1.12", "10.0.1.13"]);
});

test("selected OSS execution nodes persist through refresh navigation and reload", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await configureOss(page);
  await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
  await page.getByRole("option", { name: "10.0.1.12", exact: true }).click();
  await page.keyboard.press("Escape");
  await page.getByRole("button", { name: "刷新执行节点", exact: true }).click();
  await page
    .getByRole("navigation", { name: "主导航" })
    .getByRole("link", { name: "存储连接", exact: true })
    .click();
  await page
    .getByRole("navigation", { name: "主导航" })
    .getByRole("link", { name: "创建迁移", exact: true })
    .click();
  await page.reload();
  await page.getByRole("button", { name: /迁移选项/ }).click();
  await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
  await expect(
    page.getByRole("option", { name: "10.0.1.13", exact: true }),
  ).toHaveAttribute("aria-selected", "true");
  await expect(
    page.getByRole("option", { name: "10.0.1.12", exact: true }),
  ).toHaveAttribute("aria-selected", "false");
  await page.keyboard.press("Escape");
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
  await expect
    .poll(
      () => requests.filter((r) => r.path === "/api/master/add-buckets").length,
    )
    .toBe(1);
  expect(
    requests.find((r) => r.path === "/api/master/add-buckets")!.body[0]
      .eligibleWorkerNodes,
  ).toEqual(["10.0.1.13"]);
});

test("empty OSS node selection is rejected and refreshing does not silently restore it", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await configureOss(page);
  await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
  for (const ip of ["10.0.1.12", "10.0.1.13"])
    await page.getByRole("option", { name: ip, exact: true }).click();
  await page.keyboard.press("Escape");
  await page.getByRole("button", { name: "刷新执行节点", exact: true }).click();
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText(
    "请至少选择一个执行节点。",
  );
  expect(
    requests.filter((r) => r.path === "/api/master/add-buckets"),
  ).toHaveLength(0);
});

test("refresh preserves an unavailable selected node and blocks submission until it is removed", async ({
  page,
}) => {
  const requests = await mockApi(page);
  let reachable = true;
  await page.route("**/api/master/local-nodes", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          nodes: [
            {
              executionNodeIp: "10.0.1.12",
              reachable: true,
              localTransferSupported: true,
            },
            {
              executionNodeIp: "10.0.1.13",
              reachable,
              localTransferSupported: true,
            },
          ],
        },
      },
    }),
  );
  await configureOss(page);
  reachable = false;
  await page.getByRole("button", { name: "刷新执行节点", exact: true }).click();
  await expect(
    page.getByText("所选节点不可用，请重新选择", { exact: true }),
  ).toBeVisible();
  const field = page
    .getByRole("combobox", { name: "执行节点", exact: true })
    .locator("..");
  await expect(field.getByText("10.0.1.13", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText(
    "所选执行节点不可用，请刷新节点列表或重新选择。",
  );
  expect(
    requests.filter((r) => r.path === "/api/master/add-buckets"),
  ).toHaveLength(0);
});

test("execution node selection fits mobile and desktop without horizontal overflow", async ({
  page,
}, testInfo) => {
  await mockApi(page);
  await configureOss(page);
  for (const width of [390, 1440]) {
    await page.setViewportSize({ width, height: 1000 });
    const input = page.getByRole("combobox", { name: "执行节点", exact: true });
    await input.scrollIntoViewIfNeeded();
    await expect(input).toBeVisible();
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth - window.innerWidth,
      ),
    ).toBeLessThanOrEqual(1);
    await page.screenshot({
      path: testInfo.outputPath(`subtask-node-selection-${width}.png`),
      animations: "disabled",
    });
  }
});
