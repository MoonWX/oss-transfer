import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";
import { mockTaskChildren, revealChildren } from "./task-children-fixture";

function ossGroup(id = 8000, overrides: Record<string, unknown> = {}) {
  const shards = Array.from({ length: 128 }, (_, index) => ({
    ...tasks[1],
    taskIndex: 10000 + index,
    status: "COMPLETED",
    putObjectCount: 128,
    listObjectCount: 128,
    workerNode: index % 2 ? "10.0.1.13" : "10.0.1.12",
  }));
  return {
    ...tasks[1],
    taskIndex: id,
    ossTaskGroup: true,
    creationId: `creation-${id}`,
    creationStatus: "COMPLETED",
    childTaskIds: shards.map((shard) => shard.taskIndex),
    shardCount: shards.length,
    shards,
    participatingWorkerNodes: ["10.0.1.12", "10.0.1.13"],
    partial: false,
    status: "COMPLETED",
    scanComplete: true,
    listObjectCount: 16384,
    putObjectCount: 16384,
    ...overrides,
  };
}

async function status(page: Page, groups: ReturnType<typeof ossGroup>[]) {
  await mockTaskChildren(page, (id) => {
    const group = groups.find((item) => item.taskIndex === id)!;
    return {
      parentTaskId: id,
      items: group?.shards || [],
      childTaskIds: group?.childTaskIds,
      totalCount: group?.shardCount,
      partial: group?.partial,
    };
  });
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: {
        totalTasks: groups.length,
        tasks: groups.map(({ shards, ...summary }) => summary),
      },
    }),
  );
}

async function configureOss(page: Page) {
  await page.goto("/create");
  await page.getByRole("combobox", { name: "源存储连接" }).click();
  await page.getByRole("option", { name: "production", exact: true }).click();
  await page.getByRole("combobox", { name: "源存储桶" }).fill("media-archive");
  await page.getByRole("combobox", { name: "目标存储连接" }).click();
  await page.getByRole("option", { name: "backup", exact: true }).click();
  await page.getByRole("combobox", { name: "目标存储桶" }).fill("media-backup");
}

function drawer(page: Page, id = 8000) {
  return page.locator(".MuiDrawer-paper").filter({
    has: page.getByRole("heading", { name: `迁移任务 #${id}` }),
  });
}

test("one OSS migration with 128 execution shards occupies one row and opens aggregate details", async ({
  page,
}) => {
  await mockApi(page);
  const group = ossGroup();
  await status(page, [group]);
  await page.goto("/tasks");
  await expect(page.getByText("共 1 个任务", { exact: false })).toBeVisible();
  await expect(page.locator("tbody tr")).toHaveCount(1);
  await expect(page.locator("tbody tr")).toContainText("16,384 / 16,384");
  await expect(page.locator("tbody tr")).toContainText("10.0.1.12, 10.0.1.13");
  await page.getByRole("button", { name: "#8000", exact: true }).click();
  await revealChildren(page);
  await expect(
    drawer(page).getByRole("heading", { name: "子任务", exact: true }),
  ).toBeVisible();
  await expect(drawer(page).getByText(/^子任务 #/)).toHaveCount(25);
  await expect(drawer(page).getByText("本次创建的任务")).toHaveCount(0);
  await expect(drawer(page).getByRole("button", { name: /^#10/ })).toHaveCount(
    0,
  );
  await expect(
    drawer(page).getByText("子任务 #8000-1 · 10.0.1.12"),
  ).toBeVisible();
  await page.getByRole("button", { name: "关闭详情" }).click();
  await expect(page.locator("tbody tr")).toHaveCount(1);
});

test("separate creations with identical bucket endpoints remain separate migrations", async ({
  page,
}) => {
  await mockApi(page);
  await status(page, [ossGroup(8000), ossGroup(8001)]);
  await page.goto("/tasks");
  await expect(page.locator("tbody tr")).toHaveCount(2);
  for (const id of [8000, 8001]) {
    await page.getByRole("button", { name: `#${id}`, exact: true }).click();
    await expect(drawer(page, id)).toBeVisible();
    await page.getByRole("button", { name: "关闭详情" }).click();
  }
});

test("automatically queued OSS migrations can be stopped without exposing a manual start action", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await status(page, [ossGroup(8000, { status: "PENDING", autoStart: true })]);
  await page.goto("/tasks");
  await expect(page.locator("tbody tr")).toContainText("排队中");
  await page
    .getByRole("checkbox", { name: "选择任务 8000", exact: true })
    .check();
  await expect(
    page.getByRole("button", { name: "启动", exact: true }),
  ).toBeDisabled();
  await expect(
    page.getByRole("button", { name: "停止", exact: true }),
  ).toBeEnabled();
  await page.getByRole("button", { name: "#8000", exact: true }).click();
  await expect(
    drawer(page).getByRole("button", { name: "启动任务", exact: true }),
  ).toHaveCount(0);
  await expect(
    drawer(page).getByRole("button", { name: "停止任务", exact: true }),
  ).toBeEnabled();
  expect(
    requests.filter((request) => request.path === "/api/master/start"),
  ).toHaveLength(0);
});

test("OSS creation opens its public task ID instead of any child ID", async ({
  page,
}) => {
  // Multiple routes/actions share this total budget; assertions keep their own deadline.
  test.setTimeout(60_000);
  const requests = await mockApi(page);
  const group = ossGroup();
  await status(page, [group]);
  await page.route("**/api/master/add-buckets", (route) =>
    route.fulfill({
      json: { success: true, addedCount: 1, taskIds: [group.taskIndex] },
    }),
  );
  await configureOss(page);
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
  await expect(
    page.getByText(/任务自动执行，可在“迁移任务”中查看状态/),
  ).toBeVisible();
  await page.getByRole("link", { name: "查看任务", exact: true }).click();
  await expect(page).toHaveURL(/\/tasks\?taskId=8000$/);
  await expect(drawer(page)).toBeVisible();
  await expect(drawer(page).getByText("本次创建的任务")).toHaveCount(0);
  await expect(drawer(page).getByRole("button", { name: /^#10/ })).toHaveCount(
    0,
  );
  await page.reload();
  await expect(drawer(page)).toBeVisible();
  expect(
    requests.filter((request) => request.path === "/api/master/start"),
  ).toHaveLength(0);
});

test("OSS group start stop resume and delete submit only the stable public ID", async ({
  page,
}) => {
  // Multiple routes/actions share this total budget; assertions keep their own deadline.
  test.setTimeout(90_000);
  await mockApi(page);
  let group = ossGroup(8000, { status: "PENDING", scanComplete: false });
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: 1, tasks: [group] } }),
  );
  const sent: { action: string; ids: number[] }[] = [];
  for (const action of ["start", "stop", "resume", "clear"]) {
    await page.route(`**/api/master/${action}`, (route) => {
      sent.push({ action, ids: route.request().postDataJSON().taskIds });
      group = { ...group, status: action === "stop" ? "STOPPED" : "RUNNING" };
      return route.fulfill({
        json: { success: true, accepted: 1, ignored: 0, failed: 0 },
      });
    });
  }
  await page.goto("/tasks?taskId=8000");
  for (const [label, confirmation] of [
    ["启动任务", "启动"],
    ["停止任务", "停止"],
    ["继续传输", "继续传输"],
    ["停止任务", "停止"],
    ["删除任务", "删除"],
  ]) {
    await drawer(page)
      .getByRole("button", { name: label, exact: true })
      .click();
    const confirmationDialog = page.getByRole("dialog", {
      name: `${confirmation} 1 个任务？`,
      exact: true,
    });
    await expect(confirmationDialog).toBeVisible();
    await page
      .getByRole("button", { name: `确认${confirmation}`, exact: true })
      .click();
    await expect(confirmationDialog).not.toBeVisible();
  }
  expect(sent).toEqual(
    ["start", "stop", "resume", "stop", "clear"].map((action) => ({
      action,
      ids: [8000],
    })),
  );
});

for (const creationStatus of ["QUEUED", "RUNNING"]) {
  for (const taskStatus of ["PENDING", "STOPPED", "COMPLETED"]) {
    test(`${creationStatus} discovery blocks start resume and reports even with ${taskStatus} child state`, async ({
      page,
    }) => {
      await mockApi(page);
      await status(page, [
        ossGroup(8000, { status: taskStatus, creationStatus }),
      ]);
      await page.goto("/tasks");
      await page
        .getByRole("checkbox", { name: "选择任务 8000", exact: true })
        .check();
      await expect(
        page.getByRole("button", { name: "启动", exact: true }),
      ).toBeDisabled();
      await expect(
        page.getByRole("button", { name: "继续传输", exact: true }),
      ).toBeDisabled();
      await expect(page.locator("tbody tr")).toContainText(
        { PENDING: "排队中", STOPPED: "已停止", COMPLETED: "已完成" }[
          taskStatus
        ]!,
      );
      await page.getByRole("button", { name: "#8000", exact: true }).click();
      await expect(
        drawer(page).getByRole("button", { name: "启动任务", exact: true }),
      ).toHaveCount(0);
      await expect(
        drawer(page).getByRole("button", { name: "继续传输", exact: true }),
      ).toHaveCount(0);
      await expect(
        drawer(page).getByRole("button", { name: "导出任务报告", exact: true }),
      ).toBeDisabled();
    });
  }
}

test("missing worker shards retain a warning and cannot produce a complete group report", async ({
  page,
}) => {
  await mockApi(page);
  await status(page, [
    ossGroup(8000, { partial: true, shards: [], shardCount: 128 }),
  ]);
  await page.goto("/tasks?taskId=8000");
  await expect(
    drawer(page).getByText("任务信息不完整，请刷新或检查执行节点。"),
  ).toBeVisible();
  await expect(
    drawer(page).getByRole("button", { name: "导出任务报告", exact: true }),
  ).toBeDisabled();
  await revealChildren(page);
  await expect(drawer(page).getByText("尚未生成子任务")).toHaveCount(0);
});

test("a terminal group report downloads by the public ID", async ({ page }) => {
  await mockApi(page);
  await status(page, [ossGroup()]);
  let ticketId = "";
  await page.route("**/api/master/task-reports/*/download-ticket", (route) => {
    ticketId = new URL(route.request().url()).pathname.split("/").at(-2)!;
    return route.fulfill({
      json: { success: true, data: { ticket: "group-ticket" } },
    });
  });
  await page.route("**/api/master/task-reports/8000/download", (route) =>
    route.fulfill({
      contentType: "application/zip",
      headers: {
        "Content-Disposition": 'attachment; filename="task-8000-report.zip"',
      },
      body: Buffer.from("UEsFBgAAAAAAAAAAAAAAAAAAAAAAAA==", "base64"),
    }),
  );
  await page.goto("/tasks?taskId=8000");
  const downloadReady = page.waitForEvent("download");
  await drawer(page)
    .getByRole("button", { name: "导出任务报告", exact: true })
    .click();
  expect((await downloadReady).suggestedFilename()).toBe(
    "task-8000-report.zip",
  );
  expect(ticketId).toBe("8000");
});

test("synchronous duplicate form submit events send only one creation request", async ({
  page,
}) => {
  await mockApi(page);
  let requests = 0;
  await page.route("**/api/master/add-buckets", async (route) => {
    requests++;
    await route.fulfill({
      json: { success: true, addedCount: 1, taskIds: [1047] },
    });
  });
  await configureOss(page);
  await page
    .getByRole("button", { name: "创建迁移任务", exact: true })
    .evaluate((button) => {
      const form = button.closest("form")!;
      form.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      );
      form.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      );
    });
  await expect(
    page.getByRole("link", { name: "查看任务", exact: true }),
  ).toBeVisible();
  expect(requests).toBe(1);
});

test("legacy child-task links resolve to their public group and operate on that ID", async ({
  page,
}) => {
  await mockApi(page);
  await status(page, [ossGroup(8000, { status: "RUNNING" })]);
  let stopped: number[] = [];
  await page.route("**/api/master/stop", (route) => {
    stopped = route.request().postDataJSON().taskIds;
    return route.fulfill({
      json: { success: true, accepted: 1, ignored: 0, failed: 0 },
    });
  });
  await page.goto("/tasks?taskId=10127");
  await expect(page).toHaveURL(/taskId=8000$/);
  await expect(drawer(page)).toBeVisible();
  await drawer(page)
    .getByRole("button", { name: "停止任务", exact: true })
    .click();
  await page.getByRole("button", { name: "确认停止", exact: true }).click();
  await expect.poll(() => stopped).toEqual([8000]);
});
