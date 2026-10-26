import { expect, test, type Locator, type Page } from "@playwright/test";
import { advanceUntilVisible, installPausedClock } from "./clock";
import { mockApi, tasks } from "./fixtures";
import { mockTaskChildren, revealChildren } from "./task-children-fixture";

type Direction = "OSS_TO_OSS" | "OSS_TO_LOCAL" | "LOCAL_TO_OSS";
type TaskRecord = Record<string, any>;

function fixture(direction: Direction, parentId = 821) {
  const sourceEndpoint =
    direction === "LOCAL_TO_OSS"
      ? { type: "LOCAL", localPath: "/mnt/source" }
      : { type: "OSS", s3Alias: "production", bucket: "source-bucket" };
  const destinationEndpoint =
    direction === "OSS_TO_LOCAL"
      ? { type: "LOCAL", localPath: "/mnt/destination" }
      : { type: "OSS", s3Alias: "backup", bucket: "destination-bucket" };
  const children: TaskRecord[] = [0, 1].map((index) => ({
    direction,
    taskIndex: direction === "OSS_TO_OSS" ? 8200 + index : -8200 - index,
    parentTaskId: parentId,
    shardId: `${parentId}-${direction === "LOCAL_TO_OSS" ? "local" : "oss"}-${index}`,
    shardIndex: index,
    status: index === 0 ? "COMPLETED" : "PARTIAL_FAILED",
    sourceEndpoint,
    destinationEndpoint,
    sourceS3Alias: direction === "LOCAL_TO_OSS" ? undefined : "production",
    sourceBucket: direction === "LOCAL_TO_OSS" ? undefined : "source-bucket",
    destS3Alias: direction === "OSS_TO_LOCAL" ? undefined : "backup",
    destBucket: direction === "OSS_TO_LOCAL" ? undefined : "destination-bucket",
    executionNodeIp: index === 0 ? "10.0.1.12" : "10.0.1.13",
    workerNode: index === 0 ? "10.0.1.12" : "10.0.1.13",
    listObjectCount: 42,
    listObjectSize: 42 * 1024 ** 2,
    putObjectCount: index === 0 ? 42 : 37,
    putObjectSize: (index === 0 ? 42 : 37) * 1024 ** 2,
    skippedObjectCount: index === 0 ? 0 : 3,
    skippedObjectSize: (index === 0 ? 0 : 3) * 1024 ** 2,
    failedObjectCount: index === 0 ? 0 : 2,
    failedObjectSize: (index === 0 ? 0 : 2) * 1024 ** 2,
    startKey: `child-${index + 1}/000.bin`,
    endKey: `child-${index + 1}/999.bin`,
    startTime: "2026-09-24 09:01:02",
    endTime: "2026-09-24 09:03:04",
    errorMessage: index === 0 ? undefined : "读取 child-2/failed.bin 失败",
  }));
  const parent: TaskRecord = {
    ...tasks[0],
    ...children[0],
    taskIndex: parentId,
    parentTaskId: undefined,
    shardId: undefined,
    shardIndex: undefined,
    status: "PARTIAL_FAILED",
    listObjectCount: 999,
    putObjectCount: 900,
    putObjectSize: 900 * 1024 ** 2,
    failedObjectCount: 2,
    participatingWorkerNodes: ["10.0.1.12", "10.0.1.13"],
    ossTaskGroup: direction === "OSS_TO_OSS",
    creationStatus: "COMPLETED",
    scanComplete: true,
    childTaskIds: children.map((child) => child.taskIndex),
    shardCount: children.length,
    shards: [...children].reverse(),
  };
  return { parent, children, partial: false, error: "", reads: 0 };
}

async function arrange(page: Page, direction: Direction, parentId = 821) {
  const requests = await mockApi(page);
  const data = fixture(direction, parentId);
  await page.route("**/api/master/status", (route) => {
    data.reads++;
    return route.fulfill({
      json: {
        totalTasks: 1,
        tasks: [
          {
            ...data.parent,
            shards: undefined,
            partial: data.partial,
          },
        ],
      },
    });
  });
  await page.route("**/api/master/local-transfers/status?*", (route) => {
    data.reads++;
    return route.fulfill({
      status: data.error ? 503 : 200,
      json: data.error
        ? { success: false, message: data.error }
        : {
            success: true,
            data: {
              tasks: [data.parent],
              shards: [...data.children].reverse(),
              partial: data.partial,
              unreachableWorkers: data.partial ? ["10.0.1.13"] : [],
            },
          },
    });
  });
  await mockTaskChildren(page, () => ({
    parentTaskId: data.parent.taskIndex,
    items: data.children,
    childTaskIds: data.parent.childTaskIds,
    partial: data.partial,
    error: data.error,
  }));
  return { data, requests };
}

function drawer(page: Page) {
  return page.locator(".MuiDrawer-paper").filter({
    has: page.getByRole("button", { name: "关闭详情", exact: true }),
  });
}

async function expectChild(page: Page, id = "821-2") {
  await expect(
    page.getByRole("heading", { name: `子任务 #${id}`, exact: true }),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "返回主任务" })).toBeVisible();
  const url = new URL(page.url());
  expect(url.searchParams.get("taskId")).toBe(id.split("-")[0]);
  expect(url.searchParams.get("childTaskId")).toBe(id);
  return page
    .getByRole("heading", { name: `子任务 #${id}`, exact: true })
    .locator("..");
}

async function expectReadOnly(child: Locator) {
  for (const name of [
    "停止",
    "删除",
    "继续传输",
    "重试失败项",
    "重试任务",
    "启动",
    "导出任务报告",
    "调整",
  ])
    await expect(child.getByRole("button", { name, exact: true })).toHaveCount(
      0,
    );
}

for (const direction of [
  "OSS_TO_OSS",
  "OSS_TO_LOCAL",
  "LOCAL_TO_OSS",
] as const) {
  test(`${direction} child card opens its own read-only execution details`, async ({
    page,
  }) => {
    const { requests } = await arrange(page, direction);
    await page.goto("/tasks?taskId=821");
    await revealChildren(page);
    const card = page.getByRole("button", { name: "查看子任务 821-2" });
    await expect(card).toBeVisible();
    await card.click();
    const child = await expectChild(page);
    expect(new URL(page.url()).searchParams.get("childNode")).toBe("10.0.1.13");
    await expect(
      child.getByText("agent (10.0.1.13)", { exact: true }),
    ).toBeVisible();
    await expect(child.getByText(/^37\s*\/\s*37 MiB$/)).toBeVisible();
    await expect(child.getByText(/^3\s*\/\s*3 MiB$/)).toBeVisible();
    await expect(child.getByText(/^2\s*\/\s*2 MiB$/)).toBeVisible();
    await expect(child.getByText(/^900\s*\/\s*900 MiB$/)).toBeHidden();
    await expect(
      child.getByText("child-2/000.bin", { exact: true }),
    ).toBeVisible();
    await expect(
      child.getByText("child-2/999.bin", { exact: true }),
    ).toBeVisible();
    await expect(
      child.getByText("2026/9/24 09:01:02", { exact: true }),
    ).toBeVisible();
    await expect(
      child.getByText("2026/9/24 09:03:04", { exact: true }),
    ).toBeVisible();
    await expect(child.getByText("读取 child-2/failed.bin 失败")).toBeVisible();
    await expect(
      child.getByText(
        direction === "LOCAL_TO_OSS"
          ? "/mnt/source"
          : "production/source-bucket",
        { exact: true },
      ),
    ).toBeVisible();
    await expect(
      child.getByText(
        direction === "OSS_TO_LOCAL"
          ? "/mnt/destination"
          : "backup/destination-bucket",
        { exact: true },
      ),
    ).toBeVisible();
    await expectReadOnly(child);
    expect(requests.filter((request) => request.method !== "GET")).toEqual([]);
  });
}

test("child navigation supports keyboard, history, reload and returning to the parent", async ({
  page,
}) => {
  await arrange(page, "OSS_TO_LOCAL");
  await page.goto("/tasks?taskId=821");
  await revealChildren(page);
  const card = page.getByRole("button", { name: "查看子任务 821-2" });
  await card.scrollIntoViewIfNeeded();
  const parentScroll = await drawer(page).evaluate(
    (element) => element.scrollTop,
  );
  expect(parentScroll).toBeGreaterThan(0);
  await card.focus();
  await page.keyboard.press("Enter");
  await expectChild(page);
  await page.goBack();
  await expect(
    page.getByRole("heading", { name: "迁移任务 #821", exact: true }),
  ).toBeVisible();
  await expect(card).toBeVisible();
  await expect
    .poll(() => drawer(page).evaluate((element) => element.scrollTop))
    .toBeCloseTo(parentScroll, 0);
  expect(new URL(page.url()).searchParams.has("childTaskId")).toBe(false);
  await page.goForward();
  await expectChild(page);
  await page.reload();
  await expectChild(page);
  await page.getByRole("button", { name: "返回主任务" }).click();
  await expect(
    page.getByRole("heading", { name: "迁移任务 #821", exact: true }),
  ).toBeVisible();
  expect(new URL(page.url()).searchParams.has("childTaskId")).toBe(false);
  expect(new URL(page.url()).searchParams.has("childNode")).toBe(false);
  await card.focus();
  await page.keyboard.press("Space");
  await expectChild(page);
  await page.getByRole("button", { name: "关闭详情" }).click();
  await expect(page).toHaveURL(/\/tasks$/);
});

test("a child deep link opens directly and its own progress updates at the selected interval", async ({
  page,
}) => {
  const { data } = await arrange(page, "LOCAL_TO_OSS");
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  await page.goto("/tasks?taskId=821&childTaskId=821-2&childNode=10.0.1.13");
  const child = await expectChild(page);
  await expect(child.getByText(/^37\s*\/\s*37 MiB$/)).toBeVisible();
  data.children[1].putObjectCount = 41;
  data.children[1].putObjectSize = 41 * 1024 ** 2;
  await expect(child.getByText(/^41\s*\/\s*41 MiB$/)).toBeVisible();
  await expect(child.getByText(/^37\s*\/\s*37 MiB$/)).toBeHidden();
  await expectReadOnly(child);
});

for (const state of ["missing", "partial", "failed"] as const) {
  test(`a ${state} child never displays its parent's statistics`, async ({
    page,
  }) => {
    const { data } = await arrange(page, "OSS_TO_LOCAL");
    data.children = [];
    data.partial = state === "partial";
    data.error = state === "failed" ? "执行节点暂时不可达" : "";
    await page.goto("/tasks?taskId=821&childTaskId=821-2&childNode=10.0.1.13");
    const child = await expectChild(page);
    await expect(
      child.getByText(
        state === "missing"
          ? "未找到此子任务。"
          : "当前节点未返回此子任务，请刷新后重试。",
        { exact: true },
      ),
    ).toBeVisible();
    await expect(child.getByText(/^900\s*\/\s*900 MiB$/)).toBeHidden();
    await expectReadOnly(child);
    await page.getByRole("button", { name: "返回主任务" }).click();
    await expect(
      page.getByRole("heading", { name: "迁移任务 #821", exact: true }),
    ).toBeVisible();
  });
}

test("polling cannot silently replace an opened legacy child with another child", async ({
  page,
}) => {
  const { data } = await arrange(page, "LOCAL_TO_OSS");
  for (const child of data.children) {
    delete child.shardId;
    delete child.shardIndex;
    child.executionNodeIp = "10.0.1.13";
    child.workerNode = "10.0.1.13";
  }
  data.parent.childTaskIds = [];
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  await page.goto("/tasks?taskId=821");
  await revealChildren(page);
  const card = page.getByRole("button", { name: "查看子任务 821-1" });
  await card.click();
  const child = await expectChild(page, "821-1");
  const openedFirst = (await child.innerText()).includes("child-1/000.bin");
  data.children = data.children.filter((item) =>
    openedFirst
      ? item.startKey !== "child-1/000.bin"
      : item.startKey !== "child-2/000.bin",
  );
  data.partial = true;
  await expect(
    child.getByText("当前节点未返回此子任务，请刷新后重试。", { exact: true }),
  ).toBeVisible();
  await expect(
    child.getByText(openedFirst ? "child-2/000.bin" : "child-1/000.bin", {
      exact: true,
    }),
  ).toBeHidden();
});

test("child details fit a narrow viewport and long object keys", async ({
  page,
}) => {
  const { data } = await arrange(page, "OSS_TO_OSS");
  data.children[1].startKey = `archive/${"long-directory/".repeat(24)}start.bin`;
  data.children[1].errorMessage = `读取 ${"very-long-object-name".repeat(18)} 失败`;
  await page.setViewportSize({ width: 360, height: 800 });
  await page.goto("/tasks?taskId=821&childTaskId=821-2&childNode=10.0.1.13");
  const child = await expectChild(page);
  await expect(
    child.getByText(data.children[1].startKey, { exact: true }),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth - innerWidth,
    ),
  ).toBeLessThanOrEqual(1);
  expect(
    await child.evaluate(
      (element) => element.scrollWidth - element.clientWidth,
    ),
  ).toBeLessThanOrEqual(1);
});

test("a child deep link whose parent is absent finishes loading with an unavailable state", async ({
  page,
}) => {
  await mockApi(page);
  let release!: () => void;
  const loaded = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route("**/api/master/status", async (route) => {
    await loaded;
    await route.fulfill({ json: { totalTasks: 0, tasks: [] } });
  });
  await page.goto("/tasks?taskId=821&childTaskId=821-2&childNode=10.0.1.13");
  const child = await expectChild(page);
  await expect(
    child.getByText("未找到此子任务。", { exact: true }),
  ).toBeVisible();
  release();
  await expect(
    child.getByText("未找到此子任务。", { exact: true }),
  ).toBeVisible();
  await expect(child.getByText("正在加载…", { exact: true })).toHaveCount(0);
  await expect(child.getByRole("progressbar")).toHaveCount(0);
  await page.getByRole("button", { name: "返回主任务" }).click();
  await expect(
    page.getByRole("heading", { name: "迁移任务 #821" }),
  ).toBeVisible();
});

test("same-number children select the requested worker and return focus to its card", async ({
  page,
}) => {
  const { data } = await arrange(page, "OSS_TO_OSS");
  data.children[1].taskIndex = data.children[0].taskIndex;
  data.children[1].shardId = data.children[0].shardId;
  data.children[1].shardIndex = data.children[0].shardIndex;
  data.parent.childTaskIds = [data.children[0].taskIndex];
  await page.goto("/tasks?taskId=821");
  await revealChildren(page);
  for (const [node, transferred] of [
    ["10.0.1.12", 42],
    ["10.0.1.13", 37],
  ] as const) {
    const card = page
      .getByRole("button", { name: "查看子任务 821-1" })
      .filter({ hasText: node });
    await card.focus();
    await page.keyboard.press("Enter");
    const child = await expectChild(page, "821-1");
    expect(new URL(page.url()).searchParams.get("childNode")).toBe(node);
    await expect(
      child.getByText(
        `${node === "10.0.1.12" ? "master" : "agent"} (${node})`,
        { exact: true },
      ),
    ).toBeVisible();
    await expect(
      child
        .getByText("已迁移", { exact: true })
        .locator("..")
        .getByText(new RegExp(`^${transferred}\\s*\\/\\s*${transferred} MiB$`)),
    ).toBeVisible();
    await page.getByRole("button", { name: "返回主任务" }).click();
    await expect(card).toBeFocused();
  }
});

test("closing child details retains the child through the exit animation", async ({
  page,
}) => {
  await arrange(page, "OSS_TO_LOCAL");
  await installPausedClock(page);
  await page.goto("/tasks?taskId=821&childTaskId=821-2&childNode=10.0.1.13");
  await advanceUntilVisible(
    page,
    page.getByRole("heading", { name: "子任务 #821-2", exact: true }),
  );
  await page.clock.runFor(400);
  const child = await expectChild(page);
  await expect(child.getByText(/^37\s*\/\s*37 MiB$/)).toBeVisible();
  const childTitle = page.getByRole("heading", {
    name: "子任务 #821-2",
    exact: true,
  });
  const parentTitle = page.getByRole("heading", {
    name: "迁移任务 #821",
    exact: true,
  });
  await page.getByRole("button", { name: "关闭详情", exact: true }).click();
  await expect(page).toHaveURL(/\/tasks$/);
  await expect(childTitle).toBeVisible();
  await expect(child.getByText(/^37\s*\/\s*37 MiB$/)).toBeVisible();
  await expect(parentTitle).toBeHidden();
  await page.clock.runFor(100);
  await expect(childTitle).toBeVisible();
  await expect(parentTitle).toBeHidden();
  await page.clock.runFor(500);
  await expect(childTitle).toHaveCount(0);
  await expect(drawer(page)).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "查看任务 821" }),
  ).toBeVisible();
});

test.describe("localized subtask timestamps", () => {
  test.use({ locale: "en-US", timezoneId: "UTC" });
  test("formats numeric and ISO time fields while preserving date-like object keys", async ({
    page,
  }) => {
    const { data } = await arrange(page, "OSS_TO_OSS");
    const dateLikeKey = "2026-09-24T01:02:03Z";
    Object.assign(data.children[1], {
      createTime: dateLikeKey,
      startTime: Date.parse("2026-09-24T01:03:04Z"),
      dateAfter: "2026-09-22T12:00:00Z",
      dateBefore: "2026-09-25T18:00:00Z",
      objectKey: dateLikeKey,
      targetRelativePath: "2026-09-24/日本語.txt",
    });
    await page.goto("/tasks?taskId=821&childTaskId=821-2&childNode=10.0.1.13");
    const heading = page.getByRole("heading", {
      name: "Subtask #821-2",
      exact: true,
    });
    await expect(heading).toBeVisible();
    const child = heading.locator("..");
    for (const value of [
      "9/24/2026, 01:02:03",
      "9/24/2026, 01:03:04",
      "9/22/2026, 12:00:00",
      "9/25/2026, 18:00:00",
      dateLikeKey,
      "2026-09-24/日本語.txt",
    ])
      await expect(child.getByText(value, { exact: true })).toBeVisible();
    // Simulate a language preference changed in another tab while details stay open.
    await page.evaluate(() => {
      localStorage.setItem("oss-transfer-language", "ja-JP");
      window.dispatchEvent(
        new StorageEvent("storage", {
          key: "oss-transfer-language",
          newValue: "ja-JP",
        }),
      );
    });
    await expect(
      page.getByRole("heading", { name: "サブタスク #821-2", exact: true }),
    ).toBeVisible();
    for (const value of [
      "2026/9/24 1:02:03",
      "2026/9/24 1:03:04",
      "2026/9/22 12:00:00",
      "2026/9/25 18:00:00",
      dateLikeKey,
      "2026-09-24/日本語.txt",
    ])
      await expect(page.getByText(value, { exact: true })).toBeVisible();
  });
});

test("manual recovery targets one child and blocks duplicate clicks while pending", async ({
  page,
}) => {
  const { data } = await arrange(page, "OSS_TO_OSS");
  data.parent.status = "RUNNING";
  data.children[1].status = "RECOVERABLE";
  data.children[1].errorMessage = "context canceled";
  let calls = 0;
  let finish!: () => void;
  const pending = new Promise<void>((resolve) => {
    finish = resolve;
  });
  await page.route(
    "**/api/master/tasks/821/children/2/recover",
    async (route) => {
      expect(route.request().method()).toBe("POST");
      calls++;
      await pending;
      data.children[1].status = "RUNNING";
      data.children[1].errorMessage = "";
      await route.fulfill({
        json: { success: true, data: { outcome: "STARTED" } },
      });
    },
  );
  await page.goto("/tasks?taskId=821&childTaskId=821-2&childNode=10.0.1.13");
  await expect(
    page.getByText("传输已中断，等待恢复。", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText("context canceled", { exact: true })).toHaveCount(
    0,
  );
  await page.getByRole("button", { name: "立即恢复", exact: true }).dblclick();
  await expect(
    page.getByRole("button", { name: "正在检查恢复条件…", exact: true }),
  ).toBeDisabled();
  expect(calls).toBe(1);
  finish();
  await expect(page.getByText("子任务已恢复。", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("button", { name: "立即恢复", exact: true }),
  ).toHaveCount(0);
  expect(calls).toBe(1);
});

test("capacity-wait recovery stays locked across navigation until a new interrupted attempt", async ({
  page,
}) => {
  const { data } = await arrange(page, "LOCAL_TO_OSS");
  data.parent.status = "RUNNING";
  data.children[1].status = "RECOVERABLE";
  data.children[1].observedAt = 10;
  data.children[1].attempt = 1;
  let calls = 0,
    runningReads = 0;
  await page.route(/\/api\/master\/tasks\/821\/children\/2$/, (route) => {
    if (data.children[1].status === "RUNNING") runningReads++;
    return route.fulfill({
      json: {
        success: true,
        data: {
          parentTaskId: 821,
          items: [data.children[1]],
          partial: false,
        },
      },
    });
  });
  await page.route(
    "**/api/master/tasks/821/children/2/recover",
    async (route) => {
      calls++;
      await route.fulfill({
        json: {
          success: true,
          data: {
            outcome: "WAITING_CAPACITY",
            child: { ...data.children[1] },
          },
        },
      });
    },
  );
  await page.goto("/tasks?taskId=821&childTaskId=821-2&childNode=10.0.1.13");
  await page.getByRole("button", { name: "立即恢复", exact: true }).click();
  await expect(
    page.getByText("节点并发已满，有空位后会自动恢复。", { exact: true }),
  ).toBeVisible();
  const processing = page.getByRole("button", { name: "处理中…", exact: true });
  await expect(processing).toBeDisabled();
  await expect(page.getByText("子任务已恢复。", { exact: true })).toHaveCount(
    0,
  );
  await page.getByRole("button", { name: "返回主任务", exact: true }).click();
  await page.goBack();
  await expect(processing).toBeDisabled();
  expect(calls).toBe(1);
  data.children[1].status = "RUNNING";
  data.children[1].observedAt = 20;
  data.children[1].attempt = 2;
  await expect.poll(() => runningReads, { timeout: 10000 }).toBeGreaterThan(0);
  data.children[1].status = "RECOVERABLE";
  data.children[1].observedAt = 30;
  // Fresh evidence of a later interruption permits another recovery.
  await expect(
    page.getByRole("button", { name: "立即恢复", exact: true }),
  ).toBeEnabled({ timeout: 10000 });
  expect(calls).toBe(1);
});

test("a quickly interrupted new child attempt can be recovered again without observing its running state", async ({
  page,
}) => {
  const { data } = await arrange(page, "LOCAL_TO_OSS");
  data.parent.status = "RUNNING";
  data.children[1].status = "RECOVERABLE";
  data.children[1].attempt = 1;
  data.children[1].observedAt = 10;
  let calls = 0;
  await page.route("**/api/master/tasks/821/children/2/recover", (route) => {
    calls++;
    data.children[1].attempt++;
    data.children[1].observedAt += 10;
    return route.fulfill({
      json: {
        success: true,
        data: { outcome: "STARTED", child: { ...data.children[1] } },
      },
    });
  });
  await page.goto("/tasks?taskId=821&childTaskId=821-2&childNode=10.0.1.13");
  const recover = page.getByRole("button", { name: "立即恢复", exact: true });
  await recover.click();
  await expect(
    page.getByRole("button", { name: "处理中…", exact: true }),
  ).toBeDisabled();
  await expect(recover).toBeEnabled({ timeout: 10000 });
  expect(calls).toBe(1);
  await recover.click();
  await expect.poll(() => calls).toBe(2);
});

test("a definite child recovery rejection releases its lock", async ({
  page,
}) => {
  const { data } = await arrange(page, "LOCAL_TO_OSS");
  data.parent.status = "RUNNING";
  data.children[1].status = "RECOVERABLE";
  let calls = 0;
  await page.route("**/api/master/tasks/821/children/2/recover", (route) => {
    calls++;
    return route.fulfill({
      status: 409,
      json: { success: false, message: "共享目录验证失败" },
    });
  });
  await page.goto("/tasks?taskId=821&childTaskId=821-2&childNode=10.0.1.13");
  const recover = page.getByRole("button", { name: "立即恢复", exact: true });
  await recover.click();
  await expect(
    page.getByText("共享目录验证失败", { exact: true }),
  ).toBeVisible();
  await expect(recover).toBeEnabled();
  await recover.click();
  await expect(recover).toBeEnabled();
  expect(calls).toBe(2);
});

test("completed children and stopped parents do not offer manual child recovery", async ({
  page,
}) => {
  const { data } = await arrange(page, "OSS_TO_LOCAL");
  data.parent.status = "STOPPED";
  data.children[1].status = "RECOVERABLE";
  await page.goto("/tasks?taskId=821&childTaskId=821-2&childNode=10.0.1.13");
  await expectChild(page);
  await expect(
    page.getByRole("button", { name: "立即恢复", exact: true }),
  ).toHaveCount(0);
  data.parent.status = "RUNNING";
  data.children[1].status = "COMPLETED";
  await page.reload();
  await expectChild(page);
  await expect(
    page.getByRole("button", { name: "立即恢复", exact: true }),
  ).toHaveCount(0);
});
