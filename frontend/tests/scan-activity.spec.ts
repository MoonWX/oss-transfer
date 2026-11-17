import { readFile } from "node:fs/promises";
import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

type Json = Record<string, any>;

// What a scanner reading one large NFS directory reports: 86,980 names read,
// the directory's files not yet examined.
const reading = (extra: Json = {}): Json => ({
  state: "LIVE",
  observedAt: Date.UTC(2026, 10, 17, 1, 2, 3),
  ageMillis: 0,
  mode: "SORTED",
  namesRead: 86980,
  examined: 1200,
  alreadyFound: 0,
  vanished: 0,
  directory: {
    key: "data/2026",
    reading: true,
    names: 86980,
    position: 0,
    elapsedMillis: 300000,
  },
  lookupMeanMillis: null,
  oldestCallMillis: 0,
  waitingFor: "",
  idleMillis: 1000,
  ...extra,
});

async function arrange(page: Page, scanActivity?: Json) {
  await mockApi(page);
  const state = {
    task: {
      ...tasks[0],
      taskIndex: 61,
      status: "RUNNING",
      scanComplete: false,
      listObjectCount: 0,
      putObjectCount: 0,
      skippedObjectCount: 0,
      ...(scanActivity ? { scanActivity } : {}),
    } as Json,
  };
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: 1, tasks: [state.task] } }),
  );
  await page.route("**/api/master/tasks/61", (route) =>
    route.fulfill({ json: { data: state.task } }),
  );
  return state;
}

const row = (page: Page) =>
  page
    .getByRole("row")
    .filter({ has: page.getByRole("button", { name: "#61", exact: true }) });

test("the task list shows names read and entries examined while a directory is read", async ({
  page,
}) => {
  const state = await arrange(page, reading());
  await page.goto("/tasks");
  const label = row(page).getByTestId("task-scan-label");
  await expect(label).toHaveText("扫描中 · 已读取 86,980 · 已检查 1,200");
  await expect(
    page.getByRole("columnheader", { name: "已迁移 / 已发现" }),
  ).toBeVisible();
  // A report older than the rates keeps its numbers and says how old they are.
  state.task = {
    ...state.task,
    scanActivity: reading({ state: "STALE", ageMillis: 65000 }),
  };
  await page.getByRole("button", { name: "刷新任务", exact: true }).click();
  await expect(label).toHaveText(
    "扫描中 · 已读取 86,980 · 已检查 1,200 · 1 分 5 秒前",
  );
  // An older scanner, or a Master without the field, shows no numbers.
  state.task = { ...state.task, scanActivity: { state: "UNSUPPORTED" } };
  await page.getByRole("button", { name: "刷新任务", exact: true }).click();
  await expect(label).toHaveText("扫描中");
  delete state.task.scanActivity;
  await page.getByRole("button", { name: "刷新任务", exact: true }).click();
  await expect(label).toHaveText("扫描中");
});

test("task details explain what the scan is doing", async ({ page }) => {
  const state = await arrange(page, reading());
  await page.goto("/tasks?taskId=61");
  const block = page.getByTestId("scan-progress");
  await expect(page.getByText("已发现", { exact: true })).toBeVisible();
  await expect(block).toContainText("扫描进度");
  await expect(block).toContainText(
    "本轮扫描已读取 86,980 个名称，已检查 1,200 项（扫描重启后从零计）",
  );
  await expect(block).toContainText(
    "正在读取目录“data/2026”中的名称：已读取 86,980 个，用时 5 分 0 秒。读完全部名称后才会开始检查和迁移其中的文件。",
  );
  await expect(block).not.toContainText("没有新进展");
  // Examining, with entries already found or gone, a slow source call and a
  // wait for the local limit; no idle line while a wait is shown.
  state.task = {
    ...state.task,
    scanActivity: reading({
      alreadyFound: 3,
      vanished: 1,
      lookupMeanMillis: 23.4,
      oldestCallMillis: 6000,
      waitingFor: "LOCAL_OPERATION_LIMIT",
      idleMillis: 40000,
      directory: {
        key: "",
        reading: false,
        names: 86980,
        position: 1201,
        elapsedMillis: 1,
      },
    }),
  };
  await expect(block).toContainText(
    "正在检查源目录：第 1,201 项，共 86,980 项",
  );
  for (const line of [
    "其中 3 项此前已发现，未再次检查",
    "1 项在检查前已被删除，已跳过",
    "条目查询平均耗时 23 毫秒",
    "源存储已有 6 秒 未响应",
    "正在等待本节点的本地文件操作限制",
  ])
    await expect(block).toContainText(line);
  await expect(block).not.toContainText("没有新进展");
  state.task = {
    ...state.task,
    scanActivity: reading({ idleMillis: 40000, state: "STALE" }),
  };
  await expect(block).toContainText("已有 40 秒 没有新进展");
  await expect(block).toContainText("扫描进度截至");
  // A large directory is examined while it is still being read.
  state.task = {
    ...state.task,
    scanActivity: reading({
      mode: "STREAMING",
      directory: {
        key: "data",
        reading: true,
        names: 5000,
        position: 4001,
        examined: 3998,
        elapsedMillis: 61000,
      },
    }),
  };
  await expect(block).toContainText(
    "正在读取并检查目录“data”：已读取 5,000 个名称，已检查 3,998 项，用时 1 分 1 秒",
  );
  await expect(block).not.toContainText("读完全部名称后");
  state.task = { ...state.task, scanActivity: { state: "UNSUPPORTED" } };
  await expect(block).toHaveText(
    "扫描进度扫描节点版本较旧，不提供实时扫描进度",
  );
  delete state.task.scanActivity;
  await expect(block).toHaveCount(0);
});

test("the nodes dialog shows the scan progress and why a takeover waits", async ({
  page,
}) => {
  await mockApi(page);
  let scan: Json = { phase: "SCANNING", complete: false, activity: reading() };
  // Tasks offer node editing once Master describes their membership.
  const rows = tasks.map((task) =>
    task.taskIndex === 1048
      ? {
          ...task,
          executionMembership: {
            nodeRevision: 3,
            pending: false,
            nodes: [{ id: "node-a", address: "10.0.1.12", state: "ACTIVE" }],
            joining: [],
            draining: [],
          },
        }
      : task,
  );
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: rows.length, tasks: rows } }),
  );
  await page.route("**/api/master/tasks/1048/nodes", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          taskId: 1048,
          generation: 1,
          nodeRevision: 3,
          status: "RUNNING",
          direction: "OSS_TO_OSS",
          localTransfer: false,
          editable: true,
          nowSupported: true,
          pending: false,
          nodes: [
            {
              id: "node-a",
              address: "10.0.1.12",
              state: "ACTIVE",
              scanner: scan.phase === "SCANNING",
              online: true,
            },
          ],
          removedNodes: [],
          candidates: [],
          scan,
        },
      },
    }),
  );
  await page.goto("/tasks?taskId=1048");
  await page.getByRole("button", { name: "编辑节点", exact: true }).click();
  const box = page.getByRole("dialog", { name: "任务 #1048 执行节点" });
  await expect(box.getByTestId("scan-progress")).toContainText(
    "正在读取目录“data/2026”中的名称：已读取 86,980 个",
  );
  await box.getByRole("button", { name: "关闭", exact: true }).click();
  scan = { phase: "WAITING", complete: false, upgradeRequired: true };
  await page.getByRole("button", { name: "编辑节点", exact: true }).click();
  await expect(box.getByTestId("task-nodes-scan")).toHaveText(
    "其余节点版本较旧，无法接续此扫描；任一节点升级后自动继续",
  );
  await expect(box.getByTestId("scan-progress")).toHaveCount(0);
});

test("an exported running task leaves the scan activity out", async ({
  page,
}) => {
  await arrange(page, reading());
  await page.goto("/tasks");
  await row(page)
    .getByRole("checkbox", { name: "选择任务 61", exact: true })
    .check();
  const download = page.waitForEvent("download");
  await page.getByRole("button", { name: "导出 JSON", exact: true }).click();
  const exported = JSON.parse(
    await readFile((await (await download).path()) as string, "utf8"),
  );
  expect(exported).toHaveLength(1);
  expect(exported[0].taskIndex).toBe(61);
  expect(exported[0]).not.toHaveProperty("scanActivity");
});

test.describe("in English", () => {
  test.use({ locale: "en-US" });

  test("scan wording reads naturally", async ({ page }) => {
    await arrange(
      page,
      reading({
        namesRead: 1,
        examined: 1,
        alreadyFound: 1,
        vanished: 1,
        directory: {
          key: "",
          reading: true,
          names: 1,
          position: 0,
          elapsedMillis: 1000,
        },
      }),
    );
    await page.goto("/tasks?taskId=61");
    const block = page.getByTestId("scan-progress");
    await expect(block).toContainText(
      "This scan run has read 1 name and examined 1 entry (counted from zero after a restart)",
    );
    await expect(block).toContainText(
      "Reading the names in the source directory: 1 read in 1 s.",
    );
    await expect(block).toContainText(
      "Already found and not examined again: 1",
    );
    await expect(block).toContainText(
      "Deleted before examination and skipped: 1",
    );
    await expect(page.getByText("Found", { exact: true })).toBeVisible();
  });
});
