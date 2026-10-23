import { expect, test } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";
import { mockTaskChildren, revealChildren } from "./task-children-fixture";
import { advanceUntilVisible, installPausedClock } from "./clock";

async function arrange(page: import("@playwright/test").Page) {
  const requests = await mockApi(page);
  const children = Array.from({ length: 1000 }, (_, index) => ({
    ...tasks[0],
    taskIndex: 12000 + index,
    shardIndex: index,
    parentTaskId: 989,
    workerNode: "10.0.1.12",
    status: "COMPLETED",
  }));
  const parent = {
    ...tasks[0],
    taskIndex: 989,
    ossTaskGroup: true,
    status: "COMPLETED",
    scanComplete: true,
    childTaskIds: children.map((item) => item.taskIndex),
    shardCount: 1000,
  };
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { tasks: [parent], totalTasks: 1 } }),
  );
  const reads: string[] = [];
  await mockTaskChildren(page, (id) => ({
    parentTaskId: id,
    items: children,
    childTaskIds: parent.childTaskIds,
  }));
  page.on("request", (request) => {
    const path = new URL(request.url()).pathname;
    if (path.includes("/tasks/989/children"))
      reads.push(
        new URL(request.url()).pathname + new URL(request.url()).search,
      );
  });
  return { reads, requests, children };
}

test("children append on scroll, retain earlier rows and stop polling outside the list", async ({
  page,
}) => {
  await installPausedClock(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  const { reads } = await arrange(page);
  await page.goto("/tasks?taskId=989");
  await advanceUntilVisible(
    page,
    page.getByRole("heading", { name: "迁移任务 #989", exact: true }),
  );
  await page.clock.runFor(400);
  expect(reads).toEqual([]);
  await revealChildren(page);
  await expect(
    page.getByRole("button", { name: "查看子任务 989-1", exact: true }),
  ).toBeVisible();
  await expect(page.locator("[data-child-id]")).toHaveCount(25);
  expect(reads).toEqual(["/api/master/tasks/989/children?offset=0&limit=25"]);
  await expect(page.getByRole("button", { name: "下一页子任务" })).toHaveCount(
    0,
  );
  await page.getByTestId("task-children-more").scrollIntoViewIfNeeded();
  await expect(
    page.getByRole("button", { name: "查看子任务 989-26", exact: true }),
  ).toBeAttached();
  await expect(
    page.getByRole("button", { name: "查看子任务 989-1", exact: true }),
  ).toBeAttached();
  await expect(page.locator("[data-child-id]")).toHaveCount(50);
  expect(reads.at(-1)).toBe(
    "/api/master/tasks/989/children?offset=25&limit=25",
  );
  await page.getByRole("dialog").evaluate((element) => {
    element.scrollTop = 0;
  });
  await page.clock.runFor(400);
  const afterScroll = reads.length;
  await page.clock.fastForward(10000);
  expect(reads).toHaveLength(afterScroll);
});

test("far child deep links use a precise query and returning loads its own page", async ({
  page,
}) => {
  const { reads } = await arrange(page);
  await page.goto("/tasks?taskId=989&childTaskId=989-77&childNode=10.0.1.12");
  await expect(
    page.getByRole("heading", { name: "子任务 #989-77", exact: true }),
  ).toBeVisible();
  await expect(
    page
      .getByRole("dialog")
      .getByText("production/media-archive", { exact: true })
      .first(),
  ).toBeVisible();
  expect(reads.length).toBeGreaterThan(0);
  expect([...new Set(reads)]).toEqual([
    "/api/master/tasks/989/children/77?executionNodeIp=10.0.1.12",
  ]);
  await page.getByRole("button", { name: "返回主任务" }).click();
  await revealChildren(page);
  await expect(
    page.getByRole("button", { name: "查看子任务 989-77", exact: true }),
  ).toBeAttached();
  expect(reads.at(-1)).toBe(
    "/api/master/tasks/989/children?offset=75&limit=25",
  );
  await expect(page.locator("[data-child-id]")).toHaveCount(25);
});

for (const id of ["989-0", "989-NaN", "989-2-extra"]) {
  test(`invalid child link ${id} finishes without a request or permanent spinner`, async ({
    page,
  }) => {
    const { reads } = await arrange(page);
    await page.goto(`/tasks?taskId=989&childTaskId=${id}`);
    await expect(
      page.getByText("未找到此子任务。", { exact: true }),
    ).toBeVisible();
    await expect(page.getByRole("progressbar")).toHaveCount(0);
    expect(reads).toEqual([]);
  });
}

test("active subtasks use one direct filtered request and preserve both views without duplicates", async ({
  page,
}, testInfo) => {
  const { reads, children } = await arrange(page);
  children[998].status = "RUNNING";
  children[999].status = "RUNNING";
  await page.goto("/tasks?taskId=989");
  await revealChildren(page);
  await expect(
    page.getByRole("button", { name: "查看子任务 989-1", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "进行中", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "查看子任务 989-999", exact: true }),
  ).toBeVisible();
  await expect(page.locator("[data-child-id]")).toHaveCount(2);
  expect(reads.filter((url) => url.includes("view=active"))).toEqual([
    "/api/master/tasks/989/children?offset=0&limit=25&view=active",
  ]);
  expect(reads.some((url) => url.includes("offset=975"))).toBe(false);
  await page.getByTestId("task-children-region").scrollIntoViewIfNeeded();
  await page.getByTestId("task-children-region").screenshot({
    path: testInfo.outputPath("subtasks-active.png"),
  });
  await page.getByRole("button", { name: "全部", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "查看子任务 989-1", exact: true }),
  ).toBeVisible();
  await expect(page.locator("[data-child-id]")).toHaveCount(25);
});

test("returning from an appended subtask keeps its loaded predecessors and focus", async ({
  page,
}) => {
  await arrange(page);
  await page.goto("/tasks?taskId=989");
  await revealChildren(page);
  await expect(page.locator("[data-child-id]")).toHaveCount(25);
  await page.getByTestId("task-children-more").scrollIntoViewIfNeeded();
  const target = page.getByRole("button", {
    name: "查看子任务 989-30",
    exact: true,
  });
  await expect(target).toBeAttached();
  await target.click();
  await expect(
    page.getByRole("heading", { name: "子任务 #989-30", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "返回主任务", exact: true }).click();
  await expect(target).toBeFocused();
  await expect(
    page.getByRole("button", { name: "查看子任务 989-1", exact: true }),
  ).toBeAttached();
  await expect(page.locator("[data-child-id]")).toHaveCount(50);
});

test("active refresh removes completed cached rows even when its cursor advances across loaded windows", async ({
  page,
}) => {
  await installPausedClock(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  const { children, reads } = await arrange(page);
  for (let index = 900; index < 1000; index++)
    children[index].status = "RUNNING";
  await page.goto("/tasks?taskId=989");
  await advanceUntilVisible(
    page,
    page.getByRole("heading", { name: "迁移任务 #989", exact: true }),
  );
  await revealChildren(page);
  await page.getByRole("button", { name: "进行中", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "查看子任务 989-901", exact: true }),
  ).toBeVisible();
  await page.getByTestId("task-children-more").scrollIntoViewIfNeeded();
  await expect(
    page.getByRole("button", { name: "查看子任务 989-926", exact: true }),
  ).toBeAttached();
  for (let index = 900; index < 998; index++)
    children[index].status = "COMPLETED";
  await page.clock.runFor(1200);
  await expect(page.locator("[data-child-id]")).toHaveCount(2);
  await expect(
    page.getByRole("button", { name: "查看子任务 989-901", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "查看子任务 989-999", exact: true }),
  ).toBeAttached();
  expect(
    reads.filter((url) => url.includes("view=active")).length,
  ).toBeLessThan(10);
});

test("retrying an appended-page failure requests that same cursor and retains earlier rows", async ({
  page,
}) => {
  const { reads } = await arrange(page);
  let attempts = 0;
  await page.route(
    "**/api/master/tasks/989/children?offset=25&limit=25",
    (route) => {
      attempts++;
      if (attempts === 1)
        return route.fulfill({
          status: 503,
          json: { success: false, message: "暂时无法读取子任务" },
        });
      return route.fallback();
    },
  );
  await page.goto("/tasks?taskId=989");
  await revealChildren(page);
  await expect(page.locator("[data-child-id]")).toHaveCount(25);
  await page.getByTestId("task-children-more").scrollIntoViewIfNeeded();
  await page
    .getByTestId("task-children-more")
    .getByRole("button", { name: "重试", exact: true })
    .click();
  await expect(page.locator("[data-child-id]")).toHaveCount(50);
  expect(attempts).toBe(2);
  expect(reads.filter((url) => url.endsWith("offset=0&limit=25"))).toHaveLength(
    1,
  );
  await expect(
    page.getByRole("button", { name: "查看子任务 989-1", exact: true }),
  ).toBeAttached();
});

test("leaving the child region cancels the whole refresh before its remaining page starts", async ({
  page,
}) => {
  await installPausedClock(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  const { reads } = await arrange(page);
  let release!: () => void;
  const blocked = new Promise<void>((resolve) => {
    release = resolve;
  });
  let hold = false;
  await page.route(
    "**/api/master/tasks/989/children?offset=0&limit=25",
    async (route) => {
      if (hold) await blocked;
      await route.fallback();
    },
  );
  await page.goto("/tasks?taskId=989");
  await advanceUntilVisible(
    page,
    page.getByRole("heading", { name: "迁移任务 #989", exact: true }),
  );
  await revealChildren(page);
  await expect(page.locator("[data-child-id]")).toHaveCount(25);
  await page.getByTestId("task-children-more").scrollIntoViewIfNeeded();
  await expect(page.locator("[data-child-id]")).toHaveCount(50);
  await page
    .getByRole("button", { name: "查看子任务 989-30", exact: true })
    .scrollIntoViewIfNeeded();
  await page.clock.runFor(200);
  hold = true;
  const before = reads.length;
  await page.clock.runFor(1200);
  await expect.poll(() => reads.length).toBe(before + 1);
  const cancelled = page.waitForEvent("requestfailed", {
    predicate: (request) =>
      request.url().endsWith("/tasks/989/children?offset=0&limit=25"),
  });
  await page.getByRole("dialog").evaluate((element) => {
    element.scrollTop = 0;
  });
  await cancelled;
  release();
  await page.clock.runFor(1200);
  await expect(page.getByTestId("task-children-region")).not.toBeInViewport();
  expect(reads.slice(before)).toEqual([
    "/api/master/tasks/989/children?offset=0&limit=25",
  ]);
});
