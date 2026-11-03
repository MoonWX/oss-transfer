import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";
import { mockTaskChildren, revealChildren } from "./task-children-fixture";

const GiB = 1024 ** 3;

// One 4 GiB object in flight with every small file done: count progress would
// read 99%, while bytes show how much of the task has actually moved.
async function arrange(page: Page) {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  const state = {
    task: {
      ...tasks[0],
      taskIndex: 61,
      generation: 1,
      status: "RUNNING",
      scanComplete: true,
      listObjectCount: 12709,
      listObjectSize: 10 * GiB,
      putObjectCount: 12708,
      putObjectSize: 6 * GiB,
      skippedObjectCount: 0,
      skippedObjectSize: 0,
      transferredBytes: 7 * GiB,
      inFlightBytes: (1 * GiB) as number | null,
      liveProgress: "LIVE",
      progressPercent: 70,
      progressBasis: "BYTES",
      requestRates: {
        observed: { list: 0, get: 2.4, head: 0, put: 85.2 } as Record<
          string,
          number
        > | null,
        configured: { scope: "CLUSTER", list: 100, head: 0, put: 0 },
        effective: { list: 100, head: 0, put: 1400 },
      },
    },
  };
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: {
        totalTasks: 1,
        runningTasks: 1,
        totalPutObjectSize: state.task.putObjectSize,
        totalTransferredBytes: state.task.transferredBytes,
        totalPutObjectCount: state.task.putObjectCount,
        tasks: [state.task],
      },
    }),
  );
  await page.route("**/api/master/tasks/61", (route) =>
    route.fulfill({ json: { data: state.task } }),
  );
  return state;
}

test("a large object in flight moves byte progress instead of sitting at 99%", async ({
  page,
}) => {
  const state = await arrange(page);
  await page.goto("/tasks");
  const row = page
    .getByRole("row")
    .filter({ has: page.getByRole("button", { name: "#61", exact: true }) });
  await expect(row).toContainText("7 GiB / 10 GiB");
  await expect(row).toContainText("70%");
  await expect(row).not.toContainText("99%");
  await expect(row.getByRole("progressbar")).toHaveAttribute(
    "aria-valuenow",
    "70",
  );
  await row.getByText("7 GiB / 10 GiB").hover();
  await expect(page.getByRole("tooltip")).toContainText(
    "12,708 / 12,709 个对象",
  );
  // A failed attempt drops its live bytes; the bar must not move backwards,
  // and the byte label holds the same sample so the two never disagree.
  state.task = {
    ...state.task,
    transferredBytes: 6 * GiB,
    progressPercent: 60,
  };
  await page.getByRole("button", { name: "刷新任务", exact: true }).click();
  await expect(row).toContainText("7 GiB / 10 GiB");
  await expect(row).toContainText("70%");
  state.task = {
    ...state.task,
    transferredBytes: 8 * GiB,
    progressPercent: 80,
  };
  await page.getByRole("button", { name: "刷新任务", exact: true }).click();
  await expect(row).toContainText("80%");
  // The last object commits, but a zero-byte object is still pending.
  state.task = {
    ...state.task,
    transferredBytes: 10 * GiB,
    inFlightBytes: 0,
    progressPercent: 99.9,
  };
  await page.getByRole("button", { name: "刷新任务", exact: true }).click();
  await expect(row).toContainText("99%");
  await expect(row).not.toContainText("100%");

  await page.goto("/");
  await expect(
    page.getByText("已迁移数据", { exact: true }).locator("../.."),
  ).toContainText("10 GiB");
});

test("task details show in-flight bytes and observed request rates", async ({
  page,
}) => {
  const state = await arrange(page);
  await page.goto("/tasks?taskId=61");
  const transferred = page.getByText("已迁移", { exact: true }).locator("..");
  await expect(transferred).toContainText("12,708 / 7 GiB");
  // Bytes are the data actually moved; no separate in-flight note.
  await expect(transferred).not.toContainText("传输中");
  // Request rates sit with the speeds under 执行信息, not in 传输统计.
  await expect(page.getByText("请求速率", { exact: true })).toHaveCount(1);
  const requests = page
    .getByText("平均速度", { exact: true })
    .locator("../..")
    .getByRole("table", { name: "请求速率" });
  await expect(requests).toBeVisible();
  const cells = async (name: string) =>
    (
      await requests
        .getByRole("row")
        .filter({ hasText: name })
        .getByRole("cell")
        .allTextContents()
    ).slice(1);
  expect(await requests.getByRole("columnheader").allTextContents()).toEqual([
    "次/秒",
    "LIST",
    "GET",
    "HEAD",
    "PUT",
  ]);
  expect(await cells("实时")).toEqual(["0", "2.4", "0", "85.2"]);
  // LIST and GET share one task limit, so that cell spans both columns.
  expect(await cells("任务上限")).toEqual(["100", "不限", "不限"]);
  expect(await cells("生效上限")).toEqual(["100", "不限", "1,400"]);

  // A stale sample or an unreachable Agent is unknown, never a smaller number.
  state.task = {
    ...state.task,
    transferredBytes: state.task.putObjectSize,
    inFlightBytes: null,
    liveProgress: "STALE",
    requestRates: { ...state.task.requestRates, observed: null },
  };
  await expect.poll(() => cells("实时")).toEqual(["—", "—", "—", "—"]);
  expect(await cells("生效上限")).toEqual(["100", "不限", "1,400"]);
  await expect(transferred).toContainText("12,708 / 6 GiB");
  await expect(transferred).not.toContainText("传输中");
});

const rowOf = (page: Page) =>
  page
    .getByRole("row")
    .filter({ has: page.getByRole("button", { name: "#61", exact: true }) });

async function reachNinety(page: Page) {
  const state = await arrange(page);
  state.task = {
    ...state.task,
    listObjectCount: 1,
    listObjectSize: 100 * GiB,
    putObjectCount: 0,
    putObjectSize: 0,
    transferredBytes: 90 * GiB,
    inFlightBytes: 90 * GiB,
    progressPercent: 90,
  };
  await page.goto("/tasks");
  const row = rowOf(page);
  await expect(row).toContainText("90 GiB / 100 GiB");
  await expect(row).toContainText("90%");
  return { state, row };
}

for (const status of ["STOPPED", "FAILED", "PARTIAL_FAILED"]) {
  test(`${status} after high in-flight progress shows the server's exact value`, async ({
    page,
  }) => {
    const { state, row } = await reachNinety(page);
    // Ending the task discards the in-flight bytes within the same generation.
    state.task = {
      ...state.task,
      status,
      transferredBytes: 0,
      inFlightBytes: 0,
      liveProgress: "NONE",
      progressPercent: 0,
    };
    await page.getByRole("button", { name: "刷新任务", exact: true }).click();
    await expect(row).toContainText("0 B / 100 GiB");
    await expect(row).toContainText("0%");
    await expect(row).not.toContainText("90%");
    await expect(row.getByRole("progressbar")).toHaveAttribute(
      "aria-valuenow",
      "0",
    );
  });
}

test("a stale sample drops the stored peak so later live samples start fresh", async ({
  page,
}) => {
  const { state, row } = await reachNinety(page);
  state.task = {
    ...state.task,
    transferredBytes: 0,
    inFlightBytes: null,
    liveProgress: "STALE",
    progressPercent: 0,
  };
  await page.getByRole("button", { name: "刷新任务", exact: true }).click();
  await expect(row).toContainText("0 B / 100 GiB");
  await expect(row).toContainText("0%");
  state.task = {
    ...state.task,
    transferredBytes: 5 * GiB,
    inFlightBytes: 5 * GiB,
    liveProgress: "LIVE",
    progressPercent: 5,
  };
  await page.getByRole("button", { name: "刷新任务", exact: true }).click();
  await expect(row).toContainText("5 GiB / 100 GiB");
  await expect(row).toContainText("5%");
});

test("a failover reassignment restarts the child shard line from zero", async ({
  page,
}) => {
  const state = await arrange(page);
  const child = {
    ...state.task,
    parentTaskId: 61,
    childTaskId: "61-1",
    shardId: "61-1",
    subtaskNumber: 1,
    attempt: 1,
    executionNodeName: "node-a",
    listObjectCount: 1,
    listObjectSize: 100 * GiB,
    putObjectCount: 0,
    putObjectSize: 0,
    transferredBytes: 90 * GiB,
    inFlightBytes: 90 * GiB,
    progressPercent: 90,
  };
  const shards = { items: [child] };
  state.task = { ...state.task, ossTaskGroup: true, shardCount: 1 } as any;
  await mockTaskChildren(page, () => ({ parentTaskId: 61, ...shards }));
  await page.goto("/tasks?taskId=61");
  await revealChildren(page);
  const line = page.getByTestId("child-byte-progress");
  await expect(line).toHaveText("90 GiB / 100 GiB · 90%");
  // Failover moves the child to another node in the same generation and the
  // copy restarts; the old peak belongs to the discarded assignment.
  shards.items = [
    {
      ...child,
      executionNodeName: "node-b",
      transferredBytes: 1 * GiB,
      inFlightBytes: 1 * GiB,
      progressPercent: 1,
    },
  ];
  await expect(line).toHaveText("1 GiB / 100 GiB · 1%");
});
