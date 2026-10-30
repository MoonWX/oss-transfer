import { expect, test, type Page } from "@playwright/test";
import { readFile } from "node:fs/promises";
import { mockApi, storageConfigs, tasks } from "./fixtures";

async function select(page: Page, label: string, option: string) {
  await page.getByRole("combobox", { name: label }).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}

function parseCsv(text: string): string[][] {
  const rows: string[][] = [];
  let row: string[] = [],
    value = "",
    quoted = false;
  for (let i = 0; i < text.length; i++) {
    const char = text[i];
    if (char === '"') {
      if (quoted && text[i + 1] === '"') {
        value += '"';
        i++;
      } else quoted = !quoted;
    } else if (!quoted && (char === "," || char === "\r" || char === "\n")) {
      row.push(value);
      value = "";
      if (char !== ",") {
        rows.push(row);
        row = [];
        if (char === "\r" && text[i + 1] === "\n") i++;
      }
    } else value += char;
  }
  if (quoted) throw new Error("Unterminated CSV field");
  if (row.length || value) rows.push([...row, value]);
  return rows;
}

test("storage read retry and manual refresh restore current connections without writing configuration", async ({
  page,
}) => {
  const requests = await mockApi(page);
  let ready = false,
    reads = 0;
  let configs = storageConfigs;
  await page.route("**/api/master/s3-config", async (route) => {
    expect(route.request().method()).toBe("GET");
    reads++;
    await route.fulfill(
      ready
        ? {
            json: {
              success: true,
              configs,
            },
          }
        : { status: 503, json: { message: "连接清单暂不可用" } },
    );
  });
  await page.goto("/storage");
  const error = page.getByRole("alert").filter({ hasText: "连接清单暂不可用" });
  await expect(error).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "暂无存储连接", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("heading", { name: "production", exact: true }),
  ).toHaveCount(0);
  const beforeRetry = reads;
  ready = true;
  await error.getByRole("button", { name: "重试", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "production", exact: true }),
  ).toBeVisible();
  await expect(error).toHaveCount(0);
  expect(reads).toBe(beforeRetry + 1);
  configs = [storageConfigs[1]];
  const beforeRefresh = reads;
  await page.getByRole("button", { name: "刷新连接", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "production", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("heading", { name: "backup", exact: true }),
  ).toBeVisible();
  expect(reads).toBe(beforeRefresh + 1);
  ready = false;
  await page.getByRole("button", { name: "刷新连接", exact: true }).click();
  await expect(error).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "backup", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "暂无存储连接", exact: true }),
  ).toHaveCount(0);
  ready = true;
  configs = [];
  await error.getByRole("button", { name: "重试", exact: true }).click();
  await expect(error).toHaveCount(0);
  await expect(
    page.getByRole("heading", { name: "暂无存储连接", exact: true }),
  ).toBeVisible();
  expect(requests.filter((item) => item.method !== "GET")).toEqual([]);
});

test("OSS exception filters submit only applied bucket and limit criteria and isolate the chosen node", async ({
  page,
}) => {
  await mockApi(page);
  const queries: Record<string, unknown>[] = [];
  await page.route("**/api/master/failed", async (route) => {
    expect(route.request().method()).toBe("POST");
    queries.push(route.request().postDataJSON());
    await route.fulfill({
      json: {
        success: true,
        failedKeys: {
          "10.0.1.12": [
            {
              taskIndex: 700,
              key: "node-a.bin",
              sourceBucket: "source",
              destBucket: "target",
            },
          ],
          "10.0.1.13": [
            {
              taskIndex: 700,
              key: "node-b.bin",
              sourceBucket: "source",
              destBucket: "target",
            },
          ],
          "10.0.1.12Count": 1,
          "10.0.1.13Count": 1,
        },
      },
    });
  });
  await page.goto("/exceptions");
  await expect(page.getByText("node-a.bin", { exact: true })).toBeVisible();
  const before = queries.length;
  await page
    .getByRole("combobox", { name: "所属任务", exact: true })
    .fill("700");
  await page
    .getByRole("textbox", { name: "源 Bucket", exact: true })
    .fill(" source ");
  await page
    .getByRole("textbox", { name: "目标 Bucket", exact: true })
    .fill(" target ");
  await select(page, "执行节点", "agent (10.0.1.13)");
  await select(page, "每节点读取上限", "5,000 条");
  expect(queries).toHaveLength(before);
  await page.getByRole("button", { name: "应用筛选", exact: true }).click();
  await expect(page.getByText("node-a.bin", { exact: true })).toHaveCount(0);
  await expect(page.getByText("node-b.bin", { exact: true })).toBeVisible();
  expect(queries.at(-1)).toEqual({
    taskIndex: 700,
    executionNodeIp: "10.0.1.13",
    sourceBucket: "source",
    destBucket: "target",
    limit: 5000,
  });
  await page.getByRole("button", { name: "重置", exact: true }).click();
  await expect(page.getByText("node-a.bin", { exact: true })).toBeVisible();
  expect(queries.at(-1)).toEqual({ limit: 1000 });
  await expect(
    page.getByRole("combobox", { name: "所属任务", exact: true }),
  ).toHaveValue("");
});

test("local exception filters bind task node direction and encoded endpoint only on submit", async ({
  page,
}) => {
  await mockApi(page);
  const queries: URL[] = [];
  await page.route(
    "**/api/master/local-transfers/failures?**",
    async (route) => {
      const url = new URL(route.request().url());
      queries.push(url);
      await route.fulfill({
        json: {
          success: true,
          data: {
            total: 1,
            items: [
              {
                taskIndex: -700,
                parentTaskId: 700,
                direction: "LOCAL_TO_OSS",
                key: "matched.bin",
                localPath: "/mnt/数据 a&b",
                workerNode: "10.0.1.13",
              },
            ],
            partial: false,
          },
        },
      });
    },
  );
  await page.goto("/exceptions?type=local");
  await expect(page.getByText("matched.bin", { exact: true })).toBeVisible();
  const before = queries.length;
  await page
    .getByRole("combobox", { name: "所属任务", exact: true })
    .fill("700");
  await select(page, "执行节点", "agent (10.0.1.13)");
  await select(page, "迁移方向", "本地 → OSS");
  await page
    .getByRole("textbox", { name: "端点 / 路径", exact: true })
    .fill(" /mnt/数据 a&b ");
  expect(queries).toHaveLength(before);
  await page
    .getByRole("textbox", { name: "端点 / 路径", exact: true })
    .press("Enter");
  await expect.poll(() => queries.length).toBe(before + 1);
  expect(Object.fromEntries(queries.at(-1)!.searchParams)).toEqual({
    offset: "0",
    limit: "25",
    taskId: "700",
    executionNodeIp: "10.0.1.13",
    direction: "LOCAL_TO_OSS",
    endpoint: "/mnt/数据 a&b",
  });
  await expect(
    page.getByRole("button", { name: "应用筛选", exact: true }),
  ).toBeEnabled();
  await page.getByRole("button", { name: "重置", exact: true }).click();
  await expect.poll(() => queries.length).toBe(before + 2);
  expect(Object.fromEntries(queries.at(-1)!.searchParams)).toEqual({
    offset: "0",
    limit: "25",
  });
});

test("local current-page CSV contains only that server page with UTF-8 quoting and formula protection", async ({
  page,
}) => {
  await mockApi(page);
  const pageTwo = [
    {
      taskIndex: 700,
      parentTaskId: 700,
      key: '中文,"quoted"\nfile.bin',
      errorMessage: 'line one\r\nline "two",尾',
      direction: "OSS_TO_LOCAL",
    },
    {
      taskIndex: 701,
      parentTaskId: 701,
      key: '=HYPERLINK("https://example.invalid")',
      errorMessage: "@SUM(1,2)",
      direction: "OSS_TO_LOCAL",
    },
  ];
  const queries: URL[] = [];
  await page.route(
    "**/api/master/local-transfers/failures?**",
    async (route) => {
      const url = new URL(route.request().url());
      queries.push(url);
      const offset = Number(url.searchParams.get("offset"));
      const items =
        offset === 25
          ? pageTwo
          : Array.from({ length: 25 }, (_, i) => ({
              taskIndex: 600 + i,
              key: `first-page-${i}.bin`,
            }));
      await route.fulfill({
        json: { success: true, data: { total: 27, items, partial: false } },
      });
    },
  );
  await page.goto("/exceptions?type=local");
  await expect(page.getByText("1–25 / 27", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "下一页", exact: true }).click();
  await expect(page.getByText("26–27 / 27", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("button", { name: "导出当前页", exact: true }),
  ).toBeEnabled();
  expect(queries.at(-1)!.searchParams.get("offset")).toBe("25");
  const downloaded = page.waitForEvent("download");
  await page.getByRole("button", { name: "导出当前页", exact: true }).click();
  const download = await downloaded;
  expect(download.suggestedFilename()).toBe("local-records.csv");
  const bytes = await readFile((await download.path())!);
  expect([...bytes.subarray(0, 3)]).toEqual([239, 187, 191]);
  const text = bytes.toString("utf8").slice(1);
  const [header, ...rows] = parseCsv(text);
  expect(rows).toHaveLength(2);
  expect(rows.every((row) => row.length === header.length)).toBe(true);
  const records = rows.map((row) =>
    Object.fromEntries(header.map((key, i) => [key, row[i]])),
  );
  expect(header).not.toContain("taskIndex");
  expect(records.map((record) => record.parentTaskId)).toEqual(["700", "701"]);
  expect(records[0]).toMatchObject({
    key: pageTwo[0].key,
    errorMessage: pageTwo[0].errorMessage,
    direction: "OSS_TO_LOCAL",
  });
  expect(records[1]).toMatchObject({
    key: `'${pageTwo[1].key}`,
    errorMessage: "'@SUM(1,2)",
  });
  expect(text).not.toContain("first-page-");
});

test("recycle refresh reads tasks and nodes while paging and deletion preserve exact selection and recover a removed last page", async ({
  page,
}) => {
  await mockApi(page);
  let rows = Array.from({ length: 27 }, (_, i) => ({
    ...tasks[3],
    taskIndex: 7000 + i,
  }));
  let taskReads = 0,
    nodeReads = 0;
  const deletions: number[][] = [];
  await page.route("**/api/master/nodes", async (route) => {
    nodeReads++;
    await route.fallback();
  });
  await page.route("**/api/master/deleted-tasks", async (route) => {
    taskReads++;
    await route.fulfill({
      json: {
        success: true,
        deletedTasks: { "10.0.1.12": rows, "10.0.1.13": [] },
        totalCount: rows.length,
      },
    });
  });
  await page.route("**/api/master/permanent-delete", async (route) => {
    expect(route.request().method()).toBe("POST");
    const ids: number[] = route.request().postDataJSON().taskIds;
    deletions.push(ids);
    rows = rows.filter((row) => !ids.includes(row.taskIndex));
    await route.fulfill({
      json: { success: true, partial: false, accepted: ids.length, failed: 0 },
    });
  });
  await page.goto("/tools?tab=recycle");
  await expect(page.getByText("1–25 / 27", { exact: true })).toBeVisible();
  const beforeRefresh = { taskReads, nodeReads };
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect.poll(() => taskReads).toBe(beforeRefresh.taskReads + 1);
  await expect.poll(() => nodeReads).toBe(beforeRefresh.nodeReads + 1);
  await expect(
    page.getByRole("button", { name: "上一页", exact: true }),
  ).toBeDisabled();
  await page.getByRole("button", { name: "下一页", exact: true }).click();
  await expect(page.getByText("26–27 / 27", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("checkbox", { name: "选择任务 7026", exact: true }),
  ).toHaveCount(0);
  await page
    .getByRole("checkbox", { name: "选择任务 7000", exact: true })
    .check();
  await page
    .getByRole("checkbox", { name: "选择任务 7001", exact: true })
    .check();
  await expect(
    page.getByRole("button", { name: "下一页", exact: true }),
  ).toBeDisabled();
  await select(page, "每页", "50");
  await expect(page.getByText("1–27 / 27", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("checkbox", { name: "选择任务 7026", exact: true }),
  ).not.toBeChecked();
  await expect(
    page.getByRole("checkbox", { name: "选择任务 7000", exact: true }),
  ).toBeChecked();
  await select(page, "每页", "25");
  await page.getByRole("button", { name: "下一页", exact: true }).click();
  await page.getByRole("button", { name: "永久删除", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("#7000");
  await expect(dialog).toContainText("#7001");
  expect(deletions).toEqual([]);
  await dialog
    .getByRole("button", { name: "确认永久删除", exact: true })
    .click();
  await expect(dialog).toBeHidden();
  expect(deletions.map((ids) => [...ids].sort((a, b) => a - b))).toEqual([
    [7000, 7001],
  ]);
  await expect(page.getByText("1–25 / 25", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("checkbox", { name: "选择任务 7026", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "永久删除", exact: true }),
  ).toBeDisabled();
  await expect(
    page.getByRole("button", { name: "下一页", exact: true }),
  ).toBeDisabled();
});

test("successful OSS subtask import posts the original payload and opens the returned task rather than a matching older task", async ({
  page,
}) => {
  const requests = await mockApi(page);
  const imported = { ...tasks[1], taskIndex: 8700 };
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: 5, tasks: [...tasks, imported] } }),
  );
  const imports: unknown[] = [];
  await page.route("**/api/master/import", async (route) => {
    expect(route.request().method()).toBe("POST");
    imports.push(route.request().postDataJSON());
    await route.fulfill({
      json: { success: true, importedCount: 1, taskIds: [8700] },
    });
  });
  await page.goto("/create");
  const shell = await page
    .getByRole("button", { name: "账号设置" })
    .elementHandle();
  await page.getByRole("tab", { name: "批量导入", exact: true }).click();
  await select(page, "导入类型", "OSS 子任务恢复 / 导入");
  const payload = [
    {
      sourceS3Alias: "production",
      sourceBucket: "analytics-2026",
      destS3Alias: "backup",
      destBucket: "analytics-archive",
      marker: "folder/a",
      endMarker: "folder/z",
      distributionMode: "balanced",
      overwrite: false,
    },
  ];
  await page
    .getByRole("textbox", { name: "任务数据", exact: true })
    .fill(JSON.stringify(payload));
  await page.getByRole("button", { name: "导入任务", exact: true }).click();
  await expect(
    page.getByRole("link", { name: "查看任务", exact: true }),
  ).toHaveAttribute("href", "/tasks?taskId=8700");
  expect(imports).toEqual([payload]);
  expect(
    requests.some(
      (item) =>
        item.path === "/api/master/add-buckets" ||
        item.path === "/api/master/local-transfers",
    ),
  ).toBe(false);
  await page.getByRole("link", { name: "查看任务", exact: true }).click();
  await expect(page).toHaveURL(/\/tasks\?taskId=8700$/);
  await expect(
    page.getByRole("heading", { name: "迁移任务 #8700", exact: true }),
  ).toBeVisible();
  expect(await shell!.evaluate((element) => element.isConnected)).toBe(true);
});

for (const scenario of [
  {
    name: "pending scheduling is partial even when every import was accepted",
    partial: true,
    unknownCount: 0,
    taskIds: [8700, 8701],
    pendingScheduleTaskIds: [8701],
    message: "节点 10.0.1.13 已保存任务 #8701，等待调度恢复。",
  },
  {
    name: "unknown receipt counts show only confirmed task links",
    partial: false,
    unknownCount: 1,
    taskIds: [8700],
    pendingScheduleTaskIds: [],
    message: "节点 10.0.1.13 未确认接收结果，请核对后再重试。",
  },
]) {
  test(`OSS import ${scenario.name}`, async ({ page }) => {
    await mockApi(page);
    const payload = ["folder/a.bin", "folder/b.bin"].map((objectKey) => ({
      sourceS3Alias: "production",
      sourceBucket: "analytics-2026",
      destS3Alias: "backup",
      destBucket: "analytics-archive",
      objectKey,
      overwrite: false,
    }));
    await page.route("**/api/master/import", (route) =>
      route.fulfill({
        json: {
          success: true,
          partial: scenario.partial,
          importedCount: payload.length,
          unknownCount: scenario.unknownCount,
          taskIds: scenario.taskIds,
          pendingScheduleTaskIds: scenario.pendingScheduleTaskIds,
          validationErrors: [scenario.message],
        },
      }),
    );
    await page.goto("/create");
    await page.getByRole("tab", { name: "批量导入", exact: true }).click();
    await select(page, "导入类型", "OSS 子任务恢复 / 导入");
    await page
      .getByRole("textbox", { name: "任务数据", exact: true })
      .fill(JSON.stringify(payload));
    await page.getByRole("button", { name: "导入任务", exact: true }).click();
    const result = page
      .getByRole("alert")
      .filter({ hasText: "提交结果待核对" });
    await expect(result).toBeVisible();
    await expect(result).toContainText(scenario.message);
    await expect(result).toContainText(
      "部分任务可能已受理。重试前请核对任务列表，避免重复提交。",
    );
    await expect(page.getByText("迁移任务已提交", { exact: true })).toHaveCount(
      0,
    );
    if (scenario.taskIds.length === 1) {
      await expect(
        result.getByRole("link", { name: "查看任务", exact: true }),
      ).toHaveAttribute("href", "/tasks?taskId=8700");
      await expect(result.locator('a[href*="taskId=8701"]')).toHaveCount(0);
      await expect(result).toContainText("任务 ID：8700。");
    } else {
      for (const taskId of scenario.taskIds)
        await expect(
          result.getByRole("link", {
            name: `查看任务 #${taskId}`,
            exact: true,
          }),
        ).toHaveAttribute("href", `/tasks?taskId=${taskId}`);
    }
  });
}

test("overview shortcuts reach nodes storage and all tasks within the existing layout", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/local-nodes", (route) =>
    route.fulfill({ status: 503, json: { message: "节点清单暂不可用" } }),
  );
  await page.goto("/");
  const shell = await page
    .getByRole("button", { name: "账号设置" })
    .elementHandle();
  for (const [label, path, heading] of [
    ["查看节点", "/nodes", "执行节点"],
    ["管理存储连接", "/storage", "存储连接"],
    ["查看全部", "/tasks", "迁移任务"],
  ]) {
    await expect(
      page.getByRole("heading", { name: "迁移概览", exact: true }),
    ).toBeVisible();
    await page.getByRole("link", { name: label, exact: true }).click();
    await expect(page).toHaveURL(path);
    await expect(
      page.getByRole("heading", { name: heading, exact: true }),
    ).toBeVisible();
    expect(await shell!.evaluate((element) => element.isConnected)).toBe(true);
    await page.goBack();
  }
});
