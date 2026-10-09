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
  return { reads, requests };
}

test("children load on visibility, render one ordinal window and stop polling outside it", async ({
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
  await page.getByRole("button", { name: "下一页子任务" }).click();
  await expect(
    page.getByRole("button", { name: "查看子任务 989-26", exact: true }),
  ).toBeAttached();
  await expect(
    page.getByRole("button", { name: "查看子任务 989-1", exact: true }),
  ).toHaveCount(0);
  await expect(page.locator("[data-child-id]")).toHaveCount(25);
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
