import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

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
  // A failed attempt drops its live bytes; the bar must not move backwards.
  state.task = {
    ...state.task,
    transferredBytes: 6 * GiB,
    progressPercent: 60,
  };
  await page.getByRole("button", { name: "刷新任务", exact: true }).click();
  await expect(row).toContainText("6 GiB / 10 GiB");
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
  await expect(transferred.getByTestId("transfer-in-flight")).toHaveText(
    "含传输中 1 GiB",
  );
  const requests = page.getByTestId("task-request-rates");
  await expect(requests).toContainText("请求速率");
  await expect(requests).toContainText(
    "LIST 0 · GET 2.4 · HEAD 0 · PUT 85.2 次/秒",
  );
  await expect(requests).toContainText(
    "任务上限 LIST / GET 100 · HEAD 不限 · PUT 不限",
  );
  await expect(requests).toContainText(
    "生效上限 LIST / GET 100 · HEAD 不限 · PUT 1,400",
  );

  // A stale sample or an unreachable Agent is unknown, never a smaller number.
  state.task = {
    ...state.task,
    transferredBytes: state.task.putObjectSize,
    inFlightBytes: null,
    liveProgress: "STALE",
    requestRates: { ...state.task.requestRates, observed: null },
  };
  await expect(requests).toContainText("—");
  await expect(requests).not.toContainText("次/秒");
  await expect(transferred).toContainText("12,708 / 6 GiB");
  await expect(transferred.getByTestId("transfer-in-flight")).toHaveCount(0);
});
