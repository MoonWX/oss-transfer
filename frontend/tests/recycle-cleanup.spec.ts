import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

type Cleanup = {
  status: "QUEUED" | "RUNNING" | "WAITING";
  phase: "AGENTS" | "MASTER";
  recordsDeleted: number;
  completedNodes: number;
  totalNodes: number;
  error?: string;
};
const initialCleanup = (): Cleanup => ({
  status: "QUEUED",
  phase: "AGENTS",
  recordsDeleted: 0,
  completedNodes: 0,
  totalNodes: 2,
});
async function fixture(page: Page, earlier = false) {
  await mockApi(page);
  const state = {
    cleanup: undefined as Cleanup | undefined,
    purged: false,
    submissions: [] as number[][],
    polls: [] as number[][],
    pollUnavailable: false,
  };
  const task = (taskIndex: number) => ({
    taskIndex,
    sourceBucket: "source",
    destBucket: "destination",
    status: taskIndex === 1 ? "COMPLETED" : "PARTIAL_FAILED",
    putObjectCount: taskIndex === 1 ? 6156137 : 504,
    cleanup: taskIndex === 1 ? state.cleanup : undefined,
  });
  await page.route("**/api/master/deleted-tasks**", (route) => {
    const isEarlier = new URL(route.request().url()).searchParams.has(
      "beforeId",
    );
    const ids = earlier ? (isEarlier ? [1] : [1001, 1000]) : [2, 1];
    const rows = ids.filter((id) => id !== 1 || !state.purged).map(task);
    return route.fulfill({
      json: {
        success: true,
        deletedTasks: { "10.0.1.12": rows },
        queriedWorkers: ["10.0.1.12"],
        complete: true,
        totalCount: rows.length,
        nextBeforeId: earlier && !isEarlier ? 1000 : undefined,
      },
    });
  });
  await page.route("**/api/master/permanent-delete", async (route) => {
    const ids = route.request().postDataJSON().taskIds as number[];
    state.submissions.push(ids);
    state.cleanup = initialCleanup();
    await route.fulfill({
      json: {
        success: true,
        accepted: ids.length,
        failed: 0,
        ignored: 0,
        details: ids.map((taskId) => ({
          taskId,
          success: true,
          accepted: true,
          cleanup: state.cleanup,
        })),
      },
    });
  });
  await page.route("**/api/master/cleanup-status", (route) => {
    const ids = route.request().postDataJSON().taskIds as number[];
    state.polls.push(ids);
    if (state.pollUnavailable)
      return route.fulfill({ status: 503, json: { message: "Unavailable" } });
    return route.fulfill({
      json: {
        success: true,
        tasks: ids.map((taskId) => ({
          taskId,
          cleanup: state.cleanup,
          purged: state.purged,
        })),
      },
    });
  });
  return state;
}
function row(page: Page, id: number) {
  return page
    .getByRole("row")
    .filter({ has: page.getByText(`#${id}`, { exact: true }) });
}

test("cleanup admission keeps the row visible, polls progress and removes only confirmed completed cleanup", async ({
  page,
}, testInfo) => {
  await page.setViewportSize({ width: 1600, height: 1000 });
  const state = await fixture(page);
  await page.goto("/tools?tab=recycle");
  await page.getByRole("checkbox", { name: "选择任务 1", exact: true }).check();
  await page.getByRole("button", { name: "永久删除", exact: true }).click();
  const confirm = page.getByRole("button", {
    name: "确认永久删除",
    exact: true,
  });
  await confirm.dispatchEvent("click");
  await confirm.dispatchEvent("click");
  await expect(page.getByRole("dialog")).toBeHidden();
  await expect(row(page, 1)).toContainText("等待清理");
  await expect(row(page, 1).getByRole("checkbox")).toBeDisabled();
  await expect(row(page, 2).getByRole("checkbox")).toBeEnabled();
  expect(state.submissions).toEqual([[1]]);
  await expect.poll(() => state.polls.length).toBeGreaterThan(0);
  state.cleanup = {
    ...initialCleanup(),
    status: "WAITING",
    recordsDeleted: 2048,
    completedNodes: 1,
    error: "Node offline",
  };
  await expect(row(page, 1)).toContainText("等待自动继续");
  await expect(row(page, 1)).toContainText("已清理 2,048 条记录");
  await expect(row(page, 1)).toContainText("节点 1/2");
  await page
    .getByRole("checkbox", { name: "选择全部已删除任务", exact: true })
    .check();
  await page.getByRole("button", { name: "永久删除", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("任务 ID：#2");
  await page.getByRole("button", { name: "取消", exact: true }).click();
  state.cleanup = {
    ...initialCleanup(),
    status: "RUNNING",
    phase: "MASTER",
    completedNodes: 2,
    recordsDeleted: 66027,
  };
  await expect(row(page, 1)).toContainText("清理主节点记录");
  await expect(row(page, 1)).toContainText("66,027");
  await page.screenshot({
    path: testInfo.outputPath("recycle-cleanup-progress.png"),
    fullPage: true,
  });
  state.purged = true;
  await expect(row(page, 1)).toHaveCount(0);
  await expect(row(page, 2)).toContainText("504");
  await expect(
    page.getByText("已完成 1 个任务的清理。", { exact: true }),
  ).toBeVisible();
  expect(state.submissions).toEqual([[1]]);
  expect(
    state.polls.every((ids) => JSON.stringify(ids) === "[1]"),
  ).toBeTruthy();
});

test("existing cleanup survives reload and progress connection failures without another delete request", async ({
  page,
}) => {
  const state = await fixture(page);
  state.cleanup = {
    ...initialCleanup(),
    status: "WAITING",
    recordsDeleted: 8192,
  };
  await page.goto("/tools?tab=recycle");
  await expect(row(page, 1)).toContainText("8,192");
  state.pollUnavailable = true;
  await expect(
    page.getByText("暂时无法获取清理进度，恢复连接后自动刷新。", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(row(page, 1)).toBeVisible();
  await expect(row(page, 1).getByRole("checkbox")).toBeDisabled();
  await page.reload();
  await expect(row(page, 1)).toContainText("等待自动继续");
  state.pollUnavailable = false;
  state.cleanup = {
    ...initialCleanup(),
    status: "RUNNING",
    recordsDeleted: 16384,
  };
  await expect(row(page, 1)).toContainText("16,384");
  await expect(
    page.getByText("暂时无法获取清理进度，恢复连接后自动刷新。", {
      exact: true,
    }),
  ).toBeHidden();
  expect(state.submissions).toEqual([]);
});

test("cleanup progress and completion refresh earlier history rows", async ({
  page,
}) => {
  const state = await fixture(page, true);
  state.cleanup = {
    ...initialCleanup(),
    status: "RUNNING",
    recordsDeleted: 128,
  };
  await page.goto("/tools?tab=recycle");
  await page.getByRole("button", { name: "加载更早记录", exact: true }).click();
  await expect(row(page, 1)).toContainText("128");
  await expect.poll(() => state.polls.length).toBeGreaterThan(0);
  state.cleanup.recordsDeleted = 65536;
  await expect(row(page, 1)).toContainText("65,536");
  state.purged = true;
  await expect(row(page, 1)).toHaveCount(0);
  await expect(row(page, 1000)).toBeVisible();
  await expect(row(page, 1001)).toBeVisible();
  expect(state.submissions).toEqual([]);
});

test("an already-cleaned duplicate response does not start another cleanup poll", async ({
  page,
}) => {
  const state = await fixture(page);
  await page.route("**/api/master/permanent-delete", (route) => {
    state.purged = true;
    return route.fulfill({
      json: {
        success: true,
        accepted: 0,
        ignored: 1,
        failed: 0,
        details: [
          {
            taskId: 1,
            success: true,
            ignored: true,
            purged: true,
            cleanup: {
              ...initialCleanup(),
              status: "DONE",
              phase: "MASTER",
              recordsDeleted: 66027,
              completedNodes: 2,
            },
          },
        ],
      },
    });
  });
  await page.goto("/tools?tab=recycle");
  await page.getByRole("checkbox", { name: "选择任务 1", exact: true }).check();
  await page.getByRole("button", { name: "永久删除", exact: true }).click();
  await page.getByRole("button", { name: "确认永久删除", exact: true }).click();
  await expect(row(page, 1)).toHaveCount(0);
  await expect(row(page, 2)).toBeVisible();
  await expect(
    page.getByText("已开始后台清理，可离开此页面。", { exact: true }),
  ).toBeHidden();
  expect(state.polls).toEqual([]);
});
