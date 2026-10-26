import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

const failure = (id: number, key: string, retryStatus = "NOT_RETRIED") => ({
  parentTaskId: id,
  taskIndex: id,
  childTaskId: `${id}-1`,
  key,
  errorMessage: "Upload failed",
  retryStatus,
  workerNode: "10.0.1.12",
  direction: "OSS_TO_OSS",
});

async function ossFailures(page: Page, rows: ReturnType<typeof failure>[]) {
  await page.route("**/api/master/failed", (route) => {
    const taskId = route.request().postDataJSON()?.taskIndex;
    const items = taskId
      ? rows.filter((row) => row.parentTaskId === taskId)
      : rows;
    return route.fulfill({
      json: {
        success: true,
        failedKeys: { "10.0.1.12": items, "10.0.1.12Count": items.length },
        queriedWorkers: ["10.0.1.12"],
        complete: true,
        totalCount: items.length,
      },
    });
  });
  await page.route("**/api/master/status?**", (route) =>
    route.fulfill({
      json: {
        success: true,
        pageComplete: true,
        hasMore: false,
        tasks: [...new Set(rows.map((row) => row.parentTaskId))].map(
          (taskIndex) => ({
            taskIndex,
            status: "PARTIAL_FAILED",
            generation: 1,
          }),
        ),
      },
    }),
  );
}

test("OSS exception actions deduplicate parents, keep task scope, and stay outside horizontal scrolling", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1000, height: 900 });
  await mockApi(page);
  await ossFailures(page, [
    failure(41, "already-recovered.bin", "RECOVERED"),
    failure(41, "already-skipped.bin", "SKIPPED"),
    failure(42, "stopped.bin", "INTERRUPTED"),
    failure(43, "completed.bin", "RECOVERED"),
  ]);
  const parents: Record<number, { status: string; generation: number }> = {
    41: { status: "PARTIAL_FAILED", generation: 3 },
    42: { status: "STOPPED", generation: 5 },
    43: { status: "COMPLETED", generation: 2 },
  };
  await page.route(/\/api\/master\/tasks\/(41|42|43)$/, (route) => {
    const id = Number(route.request().url().split("/").at(-1));
    return route.fulfill({ json: { success: true, data: parents[id] } });
  });
  const submissions: { path: string; body: unknown }[] = [];
  await page.route(/\/api\/master\/(retry|resume)$/, (route) => {
    const body = route.request().postDataJSON();
    submissions.push({ path: new URL(route.request().url()).pathname, body });
    const id = body.taskIds[0];
    parents[id] = { status: "RUNNING", generation: parents[id].generation + 1 };
    return route.fulfill({
      json: {
        success: true,
        accepted: 1,
        ignored: 0,
        failed: 0,
        details: [
          { taskId: id, ...parents[id], accepted: true, success: true },
        ],
      },
    });
  });
  await page.goto("/exceptions");
  const actions = page.getByRole("region", { name: "任务操作", exact: true });
  await expect(actions).toHaveCount(0);
  const taskFilter = page.getByRole("combobox", {
    name: "所属任务",
    exact: true,
  });
  await taskFilter.fill("41");
  await page.getByRole("option", { name: "任务 #41", exact: true }).click();
  await expect(
    actions.getByRole("button", { name: "重试失败项", exact: true }),
  ).toBeEnabled();
  await expect(
    page.getByRole("table").getByRole("button", { name: /重试|继续传输/ }),
  ).toHaveCount(0);
  await expect(page.getByRole("table")).toContainText("already-recovered.bin");
  await expect(page.getByRole("table")).not.toContainText("stopped.bin");
  await expect(page.getByRole("table")).not.toContainText("completed.bin");

  const retry = actions.getByRole("button", {
    name: "重试失败项",
    exact: true,
  });
  await retry.scrollIntoViewIfNeeded();
  const before = await retry.boundingBox();
  expect(before).not.toBeNull();
  expect(before!.x).toBeGreaterThanOrEqual(0);
  expect(before!.x + before!.width).toBeLessThanOrEqual(1000);
  await page.locator(".MuiTableContainer-root").evaluate((element) => {
    element.scrollLeft = element.scrollWidth;
  });
  const after = await retry.boundingBox();
  expect(after!.x).toBeCloseTo(before!.x, 1);

  // Visible records may all be recovered, while another page of this task
  // still contains failures. The action belongs to the parent, not these rows.
  await retry.dblclick();
  await expect.poll(() => submissions.length).toBe(1);
  expect(submissions[0]).toEqual({
    path: "/api/master/retry",
    body: { taskIds: [41], expectedGenerations: { "41": 3 } },
  });
  await expect(
    actions.getByRole("button", { name: "处理中…", exact: true }),
  ).toBeDisabled();
  await expect(
    page.getByText("重试已受理 1，忽略 0，失败 0", { exact: true }),
  ).toBeVisible();

  await taskFilter.fill("42");
  await page.getByRole("option", { name: "任务 #42", exact: true }).click();
  await expect(page.getByRole("table")).not.toContainText(
    "already-recovered.bin",
  );
  await expect(page.getByRole("table")).toContainText("stopped.bin");
  await actions.getByRole("button", { name: "继续传输", exact: true }).click();
  await expect.poll(() => submissions.length).toBe(2);
  expect(submissions[1]).toEqual({
    path: "/api/master/resume",
    body: { taskIds: [42], expectedGenerations: { "42": 5 } },
  });
  await expect(
    page.getByText("任务 #42 的继续传输请求已受理。", { exact: true }),
  ).toBeVisible();
  await taskFilter.fill("43");
  await page.getByRole("option", { name: "任务 #43", exact: true }).click();
  await expect(
    actions.getByRole("button", { name: "任务已完成", exact: true }),
  ).toBeDisabled();
  expect(submissions).toHaveLength(2);
  await taskFilter.fill("");
  await expect(page.getByRole("table")).toContainText("already-recovered.bin");
  await expect(page.getByRole("table")).toContainText("stopped.bin");
  await expect(actions).toHaveCount(0);
});

test("page and tab changes scope actions to the newly displayed parent", async ({
  page,
}) => {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-exceptions-page-size", "10"),
  );
  await ossFailures(page, [
    ...Array.from({ length: 10 }, (_, i) => failure(101, `file-${i}.bin`)),
    failure(102, "next-page.bin"),
  ]);
  const parents: Record<number, { status: string; generation: number }> = {
    101: { status: "PARTIAL_FAILED", generation: 101 },
    102: { status: "PARTIAL_FAILED", generation: 102 },
    201: { status: "STOPPED", generation: 201 },
  };
  await page.route(/\/api\/master\/tasks\/(101|102|201)$/, (route) => {
    const id = Number(route.request().url().split("/").at(-1));
    return route.fulfill({
      json: {
        success: true,
        data: parents[id],
      },
    });
  });
  await page.route("**/api/master/local-transfers/failures?**", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          items: [{ ...failure(201, "local.bin"), direction: "OSS_TO_LOCAL" }],
          total: 1,
        },
      },
    }),
  );
  const submitted: unknown[] = [];
  await page.route("**/api/master/retry", (route) => {
    submitted.push(route.request().postDataJSON());
    parents[101] = { status: "RESUMING", generation: 102 };
    return route.fulfill({
      json: {
        success: true,
        accepted: 1,
        ignored: 0,
        failed: 0,
        details: [{ taskId: 101, ...parents[101], accepted: true }],
      },
    });
  });
  await page.route("**/api/master/resume", (route) => {
    submitted.push(route.request().postDataJSON());
    parents[201] = { status: "RESUMING", generation: 202 };
    return route.fulfill({
      json: {
        success: true,
        accepted: 1,
        ignored: 0,
        failed: 0,
        details: [
          { taskId: 201, generation: 202, status: "RESUMING", accepted: true },
        ],
      },
    });
  });
  await page.goto("/exceptions");
  const actions = page.getByRole("region", { name: "任务操作", exact: true });
  await expect(actions).toHaveCount(0);
  const taskFilter = page.getByRole("combobox", {
    name: "所属任务",
    exact: true,
  });
  await taskFilter.fill("101");
  await page.getByRole("option", { name: "任务 #101", exact: true }).click();
  await expect(actions).toContainText("任务 #101");
  await actions
    .getByRole("button", { name: "重试失败项", exact: true })
    .click();
  const retryBanner = page.getByText("任务 #101 的重试已受理。", {
    exact: true,
  });
  await expect(retryBanner).toBeVisible();
  await taskFilter.fill("");
  await expect(retryBanner).toHaveCount(0);
  await page
    .getByRole("navigation", { name: "分页", exact: true })
    .getByRole("button", { name: "下一页", exact: true })
    .click();
  await expect(page.getByRole("table")).toContainText("next-page.bin");
  await expect(actions).toHaveCount(0);
  await expect(retryBanner).toHaveCount(0);
  await page.getByRole("tab", { name: "本地迁移失败", exact: true }).click();
  await expect(actions).toContainText("任务 #201");
  await actions.getByRole("button", { name: "继续传输", exact: true }).click();
  await expect.poll(() => submitted.length).toBe(2);
  expect(submitted).toEqual([
    { taskIds: [101], expectedGenerations: { "101": 101 } },
    { taskIds: [201], expectedGenerations: { "201": 201 } },
  ]);
  const resumeBanner = page.getByText("任务 #201 的继续传输请求已受理。", {
    exact: true,
  });
  await expect(resumeBanner).toBeVisible();
  await page.getByRole("tab", { name: "降级迁移记录", exact: true }).click();
  await expect(actions).toHaveCount(0);
  await expect(resumeBanner).toHaveCount(0);
  expect(submitted).toHaveLength(2);
});

test("changing the exception tab during preflight does not submit a stale task action", async ({
  page,
}) => {
  await mockApi(page);
  await ossFailures(page, [failure(301, "first.bin")]);
  let release!: () => void;
  const delayed = new Promise<void>((resolve) => {
    release = resolve;
  });
  let reads = 0,
    submissions = 0;
  await page.route("**/api/master/tasks/301", async (route) => {
    reads++;
    if (reads === 2) await delayed;
    await route.fulfill({
      json: {
        success: true,
        data: { status: "PARTIAL_FAILED", generation: 1 },
      },
    });
  });
  await page.route("**/api/master/tasks/302", (route) =>
    route.fulfill({
      json: { success: true, data: { status: "STOPPED", generation: 2 } },
    }),
  );
  await page.route("**/api/master/local-transfers/failures?**", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: { items: [failure(302, "second.bin")], total: 1 },
      },
    }),
  );
  await page.route("**/api/master/retry", (route) => {
    submissions++;
    return route.fulfill({
      json: { success: true, accepted: 1, failed: 0, ignored: 0 },
    });
  });
  try {
    await page.goto("/exceptions");
    const actions = page.getByRole("region", { name: "任务操作", exact: true });
    await actions
      .getByRole("button", { name: "重试失败项", exact: true })
      .click();
    await expect.poll(() => reads).toBe(2);
    await page.getByRole("tab", { name: "本地迁移失败", exact: true }).click();
    await expect(actions).toContainText("任务 #302");
    release();
    await expect(
      actions.getByRole("button", { name: "继续传输", exact: true }),
    ).toBeEnabled();
    expect(submissions).toBe(0);
    await page.getByRole("tab", { name: "失败对象", exact: true }).click();
    await expect(actions).toContainText("任务 #301");
    await expect(
      actions.getByRole("button", { name: "重试失败项", exact: true }),
    ).toBeEnabled();
    expect(submissions).toBe(0);
  } finally {
    release();
  }
});

test("local task filtering resets pagination and rejects IDs that could broaden the query", async ({
  page,
}) => {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-exceptions-page-size", "10"),
  );
  const queries: URL[] = [];
  await page.route("**/api/master/status?**", (route) =>
    route.fulfill({
      json: {
        tasks: [
          { taskIndex: 101, status: "PARTIAL_FAILED", generation: 1 },
          { taskIndex: 102, status: "PARTIAL_FAILED", generation: 1 },
        ],
        hasMore: false,
        pageComplete: true,
      },
    }),
  );
  const records = [
    ...Array.from({ length: 12 }, (_, i) =>
      failure(101, `parent-101-${i}.bin`),
    ),
    ...Array.from({ length: 18 }, (_, i) =>
      failure(102, `parent-102-${i}.bin`),
    ),
  ];
  await page.route("**/api/master/local-transfers/failures?**", (route) => {
    const url = new URL(route.request().url());
    queries.push(url);
    const id = Number(url.searchParams.get("taskId"));
    const items = id
      ? records.filter((row) => row.parentTaskId === id)
      : records;
    const offset = Number(url.searchParams.get("offset"));
    const limit = Number(url.searchParams.get("limit"));
    return route.fulfill({
      json: {
        success: true,
        data: {
          items: items.slice(offset, offset + limit),
          total: items.length,
        },
      },
    });
  });
  await page.route(/\/api\/master\/tasks\/(101|102)$/, (route) =>
    route.fulfill({
      json: { data: { status: "PARTIAL_FAILED", generation: 1 } },
    }),
  );
  await page.goto("/exceptions?type=local");
  const pagination = page.getByRole("navigation", {
    name: "分页",
    exact: true,
  });
  await pagination.getByRole("button", { name: "下一页", exact: true }).click();
  await expect(pagination).toContainText("11–20 / 30");
  const filter = page.getByRole("combobox", { name: "所属任务", exact: true });
  await filter.fill("102");
  await page.getByRole("option", { name: "任务 #102", exact: true }).click();
  await expect(pagination).toContainText("1–10 / 18");
  await expect(page.getByRole("table")).not.toContainText("parent-101");
  await expect(page.getByRole("table")).toContainText("parent-102");
  expect(queries.at(-1)?.searchParams.get("taskId")).toBe("102");
  expect(queries.at(-1)?.searchParams.get("offset")).toBe("0");
  const count = queries.length;
  for (const value of ["0", "1e2", "9007199254740993"]) {
    await filter.fill(value);
    await filter.press("Escape");
    await page.getByRole("button", { name: "应用筛选", exact: true }).click();
    await expect(
      page.getByText("任务 ID 必须是正整数。", { exact: true }),
    ).toBeVisible();
    expect(queries.length).toBe(count);
  }
  await filter.fill("00101");
  await filter.press("Enter");
  await expect(pagination).toContainText("1–10 / 12");
  await expect(filter).toHaveValue("101");
  expect(queries.at(-1)?.searchParams.get("taskId")).toBe("101");
  await filter.fill("");
  await expect(pagination).toContainText("1–10 / 30");
  expect(queries.at(-1)?.searchParams.has("taskId")).toBe(false);
});
