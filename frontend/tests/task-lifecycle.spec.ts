import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";
import { revealChildren } from "./task-children-fixture";
import { advanceUntilVisible, installPausedClock } from "./clock";

async function detail(page: Page, id: number, paused = false) {
  await page.goto(`/tasks?taskId=${id}`);
  if (paused) {
    await advanceUntilVisible(
      page,
      page.getByRole("heading", { name: `迁移任务 #${id}` }),
    );
    await page.clock.runFor(400);
  }
  await expect(
    page.getByRole("heading", { name: `迁移任务 #${id}` }),
  ).toBeVisible();
}
async function createOss(page: Page) {
  await page.goto("/create");
  await page.getByRole("combobox", { name: "源存储连接" }).click();
  await page.getByRole("option", { name: "production", exact: true }).click();
  await page.getByRole("combobox", { name: "源存储桶" }).fill("media-archive");
  await page.getByRole("combobox", { name: "目标存储连接" }).click();
  await page.getByRole("option", { name: "backup", exact: true }).click();
  await page.getByRole("combobox", { name: "目标存储桶" }).fill("media-backup");
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
}
const upload = {
  ...tasks[2],
  taskIndex: 1050,
  direction: "LOCAL_TO_OSS",
  status: "STOPPED",
  sourceEndpoint: { type: "LOCAL", localPath: "/mnt/upload" },
  destinationEndpoint: { type: "OSS", s3Alias: "backup", bucket: "upload" },
};

test("creation opens the accepted task directly and preserves the shared layout", async ({
  page,
}) => {
  // Creation, direct details, reload and browser history share this total budget.
  test.setTimeout(60_000);
  await mockApi(page);
  await page.route("**/api/master/add-buckets", (route) =>
    route.fulfill({ json: { success: true, addedCount: 1, taskIds: [1047] } }),
  );
  await createOss(page);
  const shell = await page
    .getByRole("button", { name: "账号设置" })
    .elementHandle();
  await page.getByRole("link", { name: "查看任务", exact: true }).click();
  await expect(page).toHaveURL(/\/tasks\?taskId=1047$/);
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1047" }),
  ).toBeVisible();
  expect(await shell!.evaluate((element) => element.isConnected)).toBe(true);
  await page.reload();
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1047" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "关闭详情" }).click();
  await expect(page).toHaveURL(/\/tasks$/);
  await page.goBack();
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1047" }),
  ).toBeVisible();
});

test("delayed OSS creation exposes only the task IDs acknowledged by its response", async ({
  page,
}) => {
  await mockApi(page);
  let release!: () => void;
  const ready = new Promise<void>((resolve) => {
    release = resolve;
  });
  let submits = 0;
  await page.route("**/api/master/add-buckets", async (route) => {
    submits++;
    expect(route.request().postDataJSON()).toHaveLength(2);
    await ready;
    await route.fulfill({
      json: { success: true, addedCount: 2, taskIds: [1047, 1048] },
    });
  });
  await page.goto("/create");
  await page.getByRole("tab", { name: "批量导入", exact: true }).click();
  const payload = ["analytics-2026", "media-archive"].map((sourceBucket) => ({
    sourceS3Alias: "production",
    sourceBucket,
    destS3Alias: "backup",
    destBucket: "media-backup",
  }));
  await page
    .getByRole("textbox", { name: "任务数据", exact: true })
    .fill(JSON.stringify(payload));
  await page.getByRole("button", { name: "导入任务", exact: true }).click();
  await expect.poll(() => submits).toBe(1);
  await expect(
    page.getByRole("link", { name: "查看任务", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "正在提交", exact: true }),
  ).toBeDisabled();
  release();
  for (const id of [1047, 1048]) {
    await expect(
      page.getByRole("link", { name: `查看任务 #${id}`, exact: true }),
    ).toHaveAttribute("href", `/tasks?taskId=${id}`);
  }
  await page.getByRole("link", { name: "查看任务 #1048", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1048", exact: true }),
  ).toBeVisible();
  expect(submits).toBe(1);
  await page.reload();
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1048", exact: true }),
  ).toBeVisible();
});

test("delayed task details do not flash a missing result or select a similarly named older task", async ({
  page,
}) => {
  await mockApi(page);
  let release!: () => void;
  const ready = new Promise<void>((resolve) => {
    release = resolve;
  });
  let reads = 0;
  await page.route("**/api/master/tasks/1049", async (route) => {
    reads++;
    await ready;
    await route.fulfill({
      json: { success: true, data: { ...tasks[1], taskIndex: 1049 } },
    });
  });
  await page.addInitScript(() => {
    const capture = () => {
      if (document.body?.textContent?.includes("当前列表中未找到此任务"))
        (window as any).missingTaskSeen = true;
    };
    (window as any).missingTaskSeen = false;
    new MutationObserver(capture).observe(document, {
      childList: true,
      subtree: true,
      characterData: true,
    });
  });
  await page.goto("/tasks?taskId=1049");
  await expect.poll(() => reads).toBeGreaterThan(0);
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1049", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("当前列表中未找到此任务，请刷新任务状态。", { exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1047", exact: true }),
  ).toHaveCount(0);
  release();
  await expect(
    page.getByRole("button", { name: "启动任务", exact: true }),
  ).toBeVisible();
  await expect(page).toHaveURL(/taskId=1049$/);
  expect(await page.evaluate(() => (window as any).missingTaskSeen)).toBe(
    false,
  );
});

test("creation without acknowledged IDs links to the list without guessing a matching task", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/add-buckets", (route) =>
    route.fulfill({ json: { success: true, addedCount: 1, taskIds: [] } }),
  );
  await createOss(page);
  await expect(
    page.getByRole("link", { name: "查看任务", exact: true }),
  ).toHaveAttribute("href", "/tasks");
  await expect(page.getByRole("link", { name: /查看任务 #/ })).toHaveCount(0);
  await page.getByRole("link", { name: "查看任务", exact: true }).click();
  await expect(page).toHaveURL(/\/tasks$/);
  await expect(page.getByRole("heading", { name: /迁移任务 #/ })).toHaveCount(
    0,
  );
});

test("local creation uses its parent ID instead of a worker shard ID", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/local-transfers", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: { acceptedCount: 1, taskIds: [6001], parentTaskIds: [1046] },
      },
    }),
  );
  await page.goto("/create");
  await page.getByRole("button", { name: /对象存储 → 本地/ }).click();
  await page.getByRole("combobox", { name: "源存储连接" }).click();
  await page.getByRole("option", { name: "production", exact: true }).click();
  await page.getByRole("combobox", { name: "源存储桶" }).fill("media-archive");
  await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
  await page.getByRole("option", { name: /10\.0\.1\.12/ }).click();
  await page.getByLabel("目标目录绝对路径").fill("/mnt/datasets/media");
  const create = page.getByRole("button", {
    name: "创建迁移任务",
    exact: true,
  });
  await expect(create).toBeDisabled();
  await page.getByRole("button", { name: "运行预检", exact: true }).click();
  await expect(create).toBeEnabled();
  await create.click();
  await expect(
    page.getByRole("link", { name: "查看任务", exact: true }),
  ).toHaveAttribute("href", "/tasks?taskId=1046");
  await page.getByRole("link", { name: "查看任务", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1046" }),
  ).toBeVisible();
});

test("local task details exclude the parent discovery record from execution shards", async ({
  page,
}) => {
  await mockApi(page);
  const parent = { ...upload, status: "PARTIAL_FAILED", failedObjectCount: 6 };
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: 1, tasks: [parent] } }),
  );
  await page.route(
    "**/api/master/tasks/1050/children?offset=0&limit=25&**",
    (route) =>
      route.fulfill({
        json: {
          success: true,
          data: {
            parentTaskId: 1050,
            offset: 0,
            limit: 25,
            totalCount: 1,
            items: [
              {
                ...parent,
                taskIndex: -1224295820,
                parentTaskId: 1050,
                subtaskNumber: 1,
                shardId: "batch-0",
                executionNodeIp: "10.0.1.12",
              },
              {
                ...parent,
                parentTaskId: 1050,
                shardId: "discovery",
                status: "COMPLETED",
                putObjectCount: 0,
                failedObjectCount: 0,
                executionNodeIp: "10.0.1.12",
              },
            ],
            partial: false,
          },
        },
      }),
  );
  await detail(page, 1050);
  await revealChildren(page);
  const drawer = page
    .locator(".MuiDrawer-paper")
    .filter({ has: page.getByRole("heading", { name: "迁移任务 #1050" }) });
  await expect(
    drawer.getByRole("heading", { name: "子任务", exact: true }),
  ).toBeVisible();
  await expect(
    drawer.getByText("子任务 #1050-1 · master (10.0.1.12)"),
  ).toBeVisible();
  await expect(drawer.getByText(/子任务 #-/)).toHaveCount(0);
  await expect(drawer.getByText("部分失败", { exact: true })).toHaveCount(2);
  await expect(drawer.getByText("已完成", { exact: true })).toHaveCount(0);
});

test("local task details retain a legacy execution record with the parent task ID", async ({
  page,
}) => {
  await mockApi(page);
  const parent = { ...upload, status: "COMPLETED" };
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: 1, tasks: [parent] } }),
  );
  await page.route(
    "**/api/master/tasks/1050/children?offset=0&limit=25&**",
    (route) =>
      route.fulfill({
        json: {
          success: true,
          data: {
            parentTaskId: 1050,
            offset: 0,
            limit: 25,
            totalCount: 1,
            items: [
              { ...parent, subtaskNumber: 1, executionNodeIp: "10.0.1.12" },
            ],
            partial: false,
          },
        },
      }),
  );
  await detail(page, 1050);
  await revealChildren(page);
  await expect(
    page.getByRole("heading", { name: "子任务", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("子任务 #1050-1 · master (10.0.1.12)"),
  ).toBeVisible();
});

for (const result of ["populated", "empty", "partial", "failed"] as const) {
  test(`local shard details wait for their response before showing ${result} state`, async ({
    page,
  }) => {
    await mockApi(page);
    await page.route("**/api/master/status", (route) =>
      route.fulfill({ json: { totalTasks: 1, tasks: [upload] } }),
    );
    let release!: () => void;
    const responseReady = new Promise<void>((resolve) => {
      release = resolve;
    });
    await page.route(
      "**/api/master/tasks/1050/children?offset=0&limit=25&**",
      async (route) => {
        await responseReady;
        await route.fulfill({
          status: result === "failed" ? 503 : 200,
          json:
            result === "failed"
              ? { success: false, message: "子任务读取失败，请稍后重试。" }
              : {
                  success: true,
                  data: {
                    parentTaskId: 1050,
                    offset: 0,
                    limit: 25,
                    totalCount: result === "populated" ? 1 : 0,
                    items:
                      result === "populated"
                        ? [
                            {
                              ...upload,
                              taskIndex: -105001,
                              parentTaskId: 1050,
                              subtaskNumber: 1,
                              shardId: "batch-0",
                              executionNodeIp: "10.0.1.12",
                            },
                          ]
                        : [],
                    partial: result === "partial",
                  },
                },
        });
      },
    );
    await detail(page, 1050);
    await revealChildren(page);
    const drawer = page.locator(".MuiDrawer-paper").filter({
      has: page.getByRole("heading", { name: "迁移任务 #1050" }),
    });
    await expect(drawer.getByText("正在加载子任务…")).toBeVisible();
    await expect(
      drawer.getByRole("heading", { name: "子任务", exact: true }),
    ).toBeVisible();
    await expect(drawer.getByText("尚未生成子任务")).toHaveCount(0);
    release();
    await expect(drawer.getByText("正在加载子任务…")).toHaveCount(0);
    if (result === "populated") {
      await expect(
        drawer.getByRole("heading", { name: "子任务", exact: true }),
      ).toBeVisible();
      await expect(
        drawer.getByText("子任务 #1050-1 · master (10.0.1.12)"),
      ).toBeVisible();
    } else if (result === "empty") {
      await expect(
        drawer.getByRole("heading", { name: "子任务", exact: true }),
      ).toBeVisible();
      await expect(drawer.getByText("尚未生成子任务")).toBeVisible();
    } else {
      await expect(
        drawer.getByText(
          result === "partial"
            ? "任务信息不完整，请刷新或检查执行节点。"
            : "子任务读取失败，请稍后重试。",
        ),
      ).toBeVisible();
      await expect(
        drawer.getByRole("heading", { name: "子任务", exact: true }),
      ).toBeVisible();
    }
    if (result !== "empty")
      await expect(drawer.getByText("尚未生成子任务")).toHaveCount(0);
  });
}

for (const scenario of [
  { status: "FAILED", count: 0, label: "查看异常记录" },
  { status: "PARTIAL_FAILED", count: 0, label: "查看异常记录" },
  { status: "FAILED", count: 2, label: "查看失败对象" },
  { status: "COMPLETED", count: 0, label: null },
]) {
  test(`task detail exception entry for ${scenario.status} with ${scenario.count} failed objects`, async ({
    page,
  }) => {
    await mockApi(page);
    const task = {
      ...tasks[2],
      taskIndex: 1051,
      direction: "OSS_TO_LOCAL",
      status: scenario.status,
      failedObjectCount: scenario.count,
      errorMessage:
        scenario.status === "FAILED" && scenario.count === 0
          ? "SOURCE_DISCOVERY_FAILED: source object returned HTTP 404"
          : null,
    };
    await page.route("**/api/master/status", (route) =>
      route.fulfill({ json: { totalTasks: 1, tasks: [task] } }),
    );
    await page.route(
      "**/api/master/local-transfers/status?taskId=1051",
      (route) =>
        route.fulfill({
          json: { success: true, data: { tasks: [task], shards: [] } },
        }),
    );
    await detail(page, 1051);
    const drawer = page.locator(".MuiDrawer-paper").filter({
      has: page.getByRole("heading", { name: "迁移任务 #1051" }),
    });
    if (scenario.label) {
      const link = drawer.getByRole("link", {
        name: scenario.label,
        exact: true,
      });
      await expect(link).toHaveAttribute(
        "href",
        "/exceptions?taskId=1051&type=local",
      );
      await link.click();
      await expect(page).toHaveURL(/\/exceptions\?taskId=1051&type=local$/);
    } else {
      await expect(
        drawer.getByRole("link", { name: /查看失败对象|查看异常记录/ }),
      ).toHaveCount(0);
    }
  });
}

test("incomplete scans and stale counters never present a completed progress bar", async ({
  page,
}) => {
  await mockApi(page);
  let task = {
    ...tasks[0],
    listObjectCount: 4000,
    putObjectCount: 4299,
    skippedObjectCount: 0,
    scanComplete: false,
  };
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: 1, tasks: [task] } }),
  );
  await page.goto("/tasks");
  const row = page
    .getByRole("row")
    .filter({ has: page.getByRole("button", { name: "#1048", exact: true }) });
  await expect(row).toContainText("4,299 已迁移");
  await expect(row).toContainText("统计更新中");
  await expect(row).not.toContainText("100%");
  await expect(row.getByRole("progressbar")).not.toHaveAttribute(
    "aria-valuenow",
  );
  task = { ...task, listObjectCount: 4299 };
  await page.getByRole("button", { name: "刷新任务", exact: true }).click();
  await expect(row).toContainText("扫描中");
  await expect(row).not.toContainText("100%");
  task = {
    ...task,
    listObjectCount: 5000,
    scanComplete: true,
    status: "LIST_DONE",
  };
  await page.getByRole("button", { name: "刷新任务", exact: true }).click();
  await expect(row).toContainText("85%");
  await expect(row.getByRole("progressbar")).toHaveAttribute(
    "aria-valuenow",
    "86",
  );
});

for (const direction of ["OSS_TO_OSS", "OSS_TO_LOCAL", "LOCAL_TO_OSS"]) {
  test(`${direction} can stop and resume from its own detail without losing counters`, async ({
    page,
  }) => {
    await mockApi(page);
    let task = {
      ...tasks[0],
      direction,
      status: "RUNNING",
      scanComplete: false,
    };
    const sent: {
      path: string;
      ids: number[];
      expectedGenerations?: Record<number, number>;
    }[] = [];
    await page.route("**/api/master/status", (route) =>
      route.fulfill({ json: { totalTasks: 1, tasks: [task] } }),
    );
    await page.route("**/api/master/tasks/1048", (route) =>
      route.fulfill({ json: { success: true, data: task } }),
    );
    const prefix =
      direction === "OSS_TO_OSS"
        ? "/api/master"
        : "/api/master/local-transfers";
    for (const action of ["stop", "resume"]) {
      await page.route(`**${prefix}/${action}`, (route) => {
        const body = route.request().postDataJSON();
        sent.push({
          path: new URL(route.request().url()).pathname,
          ids: body.taskIds,
          ...(body.expectedGenerations
            ? { expectedGenerations: body.expectedGenerations }
            : {}),
        });
        if (action === "resume")
          expect(body.expectedGenerations).toEqual({ 1048: task.generation });
        task = {
          ...task,
          status: action === "stop" ? "STOPPED" : "RUNNING",
          generation:
            action === "resume" ? task.generation + 1 : task.generation,
        };
        const result = {
          success: true,
          accepted: 1,
          ignored: 0,
          failed: 0,
          partial: false,
          details: [
            {
              taskId: task.taskIndex,
              generation: task.generation,
              status: task.status,
              success: true,
              accepted: true,
              ignored: false,
            },
          ],
        };
        return route.fulfill({
          json:
            direction === "OSS_TO_OSS"
              ? result
              : { success: true, data: result },
        });
      });
    }
    await detail(page, 1048);
    await page.getByRole("button", { name: "停止任务", exact: true }).click();
    await page.getByRole("button", { name: "确认停止", exact: true }).click();
    await expect(
      page.getByRole("button", { name: "继续传输", exact: true }),
    ).toBeVisible();
    await expect(page.getByText("124,200").last()).toBeVisible();
    await page.getByRole("button", { name: "继续传输", exact: true }).click();
    await page
      .getByRole("button", { name: "确认继续传输", exact: true })
      .click();
    await expect(
      page.getByRole("button", { name: "停止任务", exact: true }),
    ).toBeVisible();
    await expect(page.getByText("124,200").last()).toBeVisible();
    expect(task.generation).toBe(2);
    expect(task.putObjectCount).toBe(tasks[0].putObjectCount);
    expect(sent).toEqual([
      { path: `${prefix}/stop`, ids: [1048] },
      {
        path: `${prefix}/resume`,
        ids: [1048],
        expectedGenerations: { 1048: 1 },
      },
    ]);
  });
}

test("nonrunning local tasks can be deleted alongside OSS tasks while running tasks cannot", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: 5, tasks: [...tasks, upload] } }),
  );
  await page.goto("/tasks");
  await page
    .getByRole("checkbox", { name: "选择任务 1048", exact: true })
    .check();
  await expect(
    page.getByRole("button", { name: "删除", exact: true }),
  ).toBeDisabled();
  await page.getByRole("button", { name: "取消选择", exact: true }).click();
  for (const id of [1045, 1046, 1050])
    await page
      .getByRole("checkbox", { name: `选择任务 ${id}`, exact: true })
      .check();
  await page.getByRole("button", { name: "删除", exact: true }).click();
  expect(requests.some((request) => request.path === "/api/master/clear")).toBe(
    false,
  );
  await page.getByRole("button", { name: "确认删除", exact: true }).click();
  await expect
    .poll(
      () =>
        requests.find((request) => request.path === "/api/master/clear")?.body
          .taskIds,
    )
    .toEqual([1046, 1045, 1050]);
});

test("partially accepted stop keeps failed nodes visible and never reports full success", async ({
  page,
}) => {
  await mockApi(page);
  let stopped = false;
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: {
        tasks: [
          { ...tasks[0], status: stopped ? "STOPPED" : "RUNNING" },
          {
            ...tasks[0],
            taskIndex: 1051,
            workerNode: "10.0.1.13",
            status: stopped ? "STOPPING" : "RUNNING",
          },
        ],
      },
    }),
  );
  await page.route("**/api/master/stop", (route) => {
    expect(
      route
        .request()
        .postDataJSON()
        .taskIds.slice()
        .sort((a: number, b: number) => a - b),
    ).toEqual([1048, 1051]);
    stopped = true;
    return route.fulfill({
      json: {
        success: true,
        partial: true,
        accepted: 1,
        ignored: 0,
        failed: 1,
        details: [
          {
            taskId: 1048,
            generation: 1,
            status: "STOPPED",
            success: true,
            accepted: true,
            ignored: false,
          },
          {
            taskId: 1051,
            generation: 1,
            status: "STOPPING",
            success: false,
            accepted: false,
            ignored: false,
            message: "Agent unavailable: 10.0.1.13",
          },
        ],
      },
    });
  });
  await page.goto("/tasks");
  for (const id of [1048, 1051])
    await page
      .getByRole("checkbox", { name: `选择任务 ${id}`, exact: true })
      .check();
  await page.getByRole("button", { name: "停止", exact: true }).click();
  await page.getByRole("button", { name: "确认停止", exact: true }).click();
  await expect(page.getByText(/部分操作未完成/)).toBeVisible();
  await expect(
    page
      .locator("main")
      .getByText(/部分请求未完成：Agent unavailable: 10\.0\.1\.13/),
  ).toBeVisible();
  await expect(
    page.locator("main").getByText("已受理 1，忽略 0，失败 1", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("checkbox", { name: "选择任务 1048", exact: true }),
  ).not.toBeChecked();
  await expect(
    page.getByRole("checkbox", { name: "选择任务 1051", exact: true }),
  ).toBeChecked();
  await expect(page.getByText(/停止请求已提交/)).toHaveCount(0);
});

test("failed task creation shows its error instead of opening a similarly named old task", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/add-buckets", (route) =>
    route.fulfill({
      status: 503,
      json: { success: false, message: "源存储桶读取失败" },
    }),
  );
  await createOss(page);
  await expect(
    page.getByRole("alert").filter({ hasText: "任务提交失败" }),
  ).toContainText("源存储桶读取失败");
  await expect(
    page.getByRole("link", { name: "查看任务", exact: true }),
  ).toHaveCount(0);
  await expect(page.getByRole("heading", { name: /迁移任务 #/ })).toHaveCount(
    0,
  );
  await expect(page).not.toHaveURL(/taskId=/);
});

test("pending task controls and report export fit a narrow detail panel", async ({
  page,
}, testInfo) => {
  await mockApi(page);
  await page.setViewportSize({ width: 360, height: 800 });
  await detail(page, 1047);
  await expect(
    page.getByRole("button", { name: "启动任务", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "删除任务", exact: true }),
  ).toBeVisible();
  const panel = page
    .locator(".MuiDrawer-paper")
    .filter({ has: page.getByRole("heading", { name: "迁移任务 #1047" }) });
  expect(
    await panel.evaluate(
      (element) => element.scrollWidth <= element.clientWidth,
    ),
  ).toBe(true);
  await expect(page.getByText(/历史任务可能缺少对象明细/)).toHaveCount(0);
  await page.screenshot({
    path: testInfo.outputPath("task-detail-mobile.png"),
    animations: "disabled",
  });
});

test("local recycle records keep the actual source and destination and surface partial deletion", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/deleted-tasks", (route) =>
    route.fulfill({
      json: {
        success: true,
        totalCount: 1,
        deletedTasks: {
          "10.0.1.12": [],
          "10.0.1.13": [],
          local: [
            { ...upload, participatingWorkerNodes: ["10.0.1.12", "10.0.1.13"] },
          ],
        },
      },
    }),
  );
  await page.route("**/api/master/permanent-delete", (route) =>
    route.fulfill({
      json: {
        success: true,
        partial: true,
        failed: 1,
        failedWorkers: ["10.0.1.13"],
        details: [
          {
            taskId: 1050,
            success: false,
            accepted: false,
            message: "节点暂不可用",
          },
        ],
      },
    }),
  );
  await page.goto("/tools?tab=recycle");
  const row = page.getByRole("row").filter({
    has: page.getByRole("checkbox", { name: "选择任务 1050", exact: true }),
  });
  await expect(row).toContainText("/mnt/upload");
  await expect(row).toContainText("backup/upload");
  await expect(row).toContainText("master (10.0.1.12), agent (10.0.1.13)");
  await row.getByRole("checkbox").check();
  await page.getByRole("button", { name: "永久删除", exact: true }).click();
  await page.getByRole("button", { name: "确认永久删除", exact: true }).click();
  const result = page.getByRole("alert").filter({ hasText: "已处理 1 / 1" });
  await expect(result).toContainText("已受理 0，忽略 0，失败 1");
  await result
    .getByRole("button", { name: "查看失败详情", exact: true })
    .click();
  await expect(result).toContainText("节点暂不可用");
  await expect(result).toContainText("未完成节点：agent (10.0.1.13)");
  await expect(row.getByRole("checkbox")).toBeChecked();
});

test("changing task links cannot reuse a previous delayed detail response", async ({
  page,
}) => {
  await mockApi(page);
  let releaseFirst!: () => void;
  const firstReady = new Promise<void>((resolve) => {
    releaseFirst = resolve;
  });
  let firstReads = 0;
  let firstResponded = false;
  await page.route("**/api/master/tasks/1049", async (route) => {
    firstReads++;
    await firstReady;
    await route.fulfill({
      json: {
        success: true,
        data: { ...tasks[1], taskIndex: 1049, sourceBucket: "first-source" },
      },
    });
    firstResponded = true;
  });
  await page.route("**/api/master/tasks/1050", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: { ...tasks[1], taskIndex: 1050, sourceBucket: "second-source" },
      },
    }),
  );
  await page.goto("/tasks?taskId=1049");
  await expect.poll(() => firstReads).toBeGreaterThan(0);
  await page.evaluate(() => {
    history.pushState({}, "", "/tasks?taskId=1050");
    window.dispatchEvent(new PopStateEvent("popstate"));
  });
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1050", exact: true }),
  ).toBeVisible();
  const drawer = page.locator(".MuiDrawer-paper").filter({
    has: page.getByRole("heading", { name: "迁移任务 #1050", exact: true }),
  });
  await expect(drawer).toContainText("second-source");
  releaseFirst();
  await expect.poll(() => firstResponded).toBe(true);
  await expect(drawer).not.toContainText("first-source");
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1049", exact: true }),
  ).toHaveCount(0);
  await expect(page).toHaveURL(/taskId=1050$/);
});

test("partially accepted creation keeps its acknowledged task link and diagnostic visible", async ({
  page,
}) => {
  await mockApi(page);
  const message = "节点暂不可用；已创建的任务仍可查看和继续传输。";
  await page.route("**/api/master/add-buckets", (route) =>
    route.fulfill({
      json: {
        success: true,
        partial: true,
        addedCount: 1,
        taskIds: [1047],
        rejectedCount: 0,
        validationErrors: [message],
      },
    }),
  );
  await createOss(page);
  await expect(page.getByText(message, { exact: true })).toBeVisible();
  await expect(page.getByText("提交结果待核对", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("link", { name: "查看任务", exact: true }),
  ).toHaveAttribute("href", "/tasks?taskId=1047");
  await page.getByRole("link", { name: "查看任务", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1047", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "启动任务", exact: true }),
  ).toBeVisible();
});

test("closing detail retains its content through exit and stops detail polling", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  await installPausedClock(page);
  await detail(page, 1046, true);
  const panel = page
    .locator(".MuiDrawer-paper")
    .filter({ has: page.getByRole("heading", { name: "迁移任务 #1046" }) });
  const handle = await panel.elementHandle();
  await panel.evaluate((element) => {
    const samples: string[] = [];
    (window as any).closingDetailSamples = samples;
    const record = () => {
      if (element.isConnected) samples.push(element.textContent || "");
    };
    new MutationObserver(record).observe(element, {
      subtree: true,
      childList: true,
      characterData: true,
    });
    record();
  });
  await page.getByRole("button", { name: "关闭详情", exact: true }).click();
  await expect(page).toHaveURL(/\/tasks$/);
  await page.clock.runFor(400);
  await expect
    .poll(() => handle!.evaluate((element) => element.isConnected))
    .toBe(false);
  const samples = await page.evaluate(
    () => (window as any).closingDetailSamples as string[],
  );
  expect(samples.length).toBeGreaterThan(0);
  expect(
    samples.every(
      (text) =>
        text.includes("迁移任务 #1046") && !text.includes("当前列表中未找到"),
    ),
  ).toBe(true);
  const detailReads = () =>
    requests.filter((request) => request.path.includes("/tasks/1046/children"))
      .length;
  const closedReads = detailReads();
  await page.clock.fastForward(3000);
  expect(detailReads()).toBe(closedReads);
  await page
    .getByRole("button", { name: "查看任务 1047", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1047" }),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1046" }),
  ).toHaveCount(0);
  await expect(
    page
      .locator(".MuiDrawer-paper")
      .filter({ has: page.getByRole("heading", { name: "迁移任务 #1047" }) }),
  ).not.toContainText("/mnt/datasets");
});

test("successful deletion retains confirmation and detail content until their exits finish", async ({
  page,
}) => {
  await mockApi(page);
  let removed = false;
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: {
        tasks: tasks.filter((task) => !removed || task.taskIndex !== 1045),
      },
    }),
  );
  await page.route("**/api/master/clear", (route) => {
    expect(route.request().postDataJSON()).toEqual({ taskIds: [1045] });
    removed = true;
    return route.fulfill({ json: { success: true, accepted: 1 } });
  });
  await detail(page, 1045);
  await page.getByRole("button", { name: "删除任务", exact: true }).click();
  const dialog = page.getByRole("dialog");
  const handle = await dialog.elementHandle();
  await dialog.evaluate((element) => {
    const samples: string[] = [];
    (window as any).closingConfirmationSamples = samples;
    const record = () => {
      if (element.isConnected) samples.push(element.textContent || "");
    };
    new MutationObserver(record).observe(element, {
      subtree: true,
      childList: true,
      characterData: true,
    });
    record();
  });
  await page.getByRole("button", { name: "确认删除", exact: true }).click();
  await expect
    .poll(() => handle!.evaluate((element) => element.isConnected))
    .toBe(false);
  const samples = await page.evaluate(
    () => (window as any).closingConfirmationSamples as string[],
  );
  expect(
    samples.every(
      (text) =>
        text.includes("删除 1 个任务？") &&
        text.includes("#1045") &&
        text.includes("任务记录将移至回收站") &&
        !text.includes("任务将开始传输"),
    ),
  ).toBe(true);
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1045" }),
  ).toHaveCount(0);
  await expect(
    page.getByText("当前列表中未找到此任务，请刷新任务状态。", { exact: true }),
  ).toHaveCount(0);
  await page
    .getByRole("checkbox", { name: "选择任务 1047", exact: true })
    .check();
  await page.getByRole("button", { name: "启动", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("启动 1 个任务？");
  await expect(page.getByRole("dialog")).toContainText("#1047");
  await expect(page.getByRole("dialog")).not.toContainText("#1045");
  await page.getByRole("button", { name: "取消", exact: true }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
});
