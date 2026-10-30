import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";
import { mockTaskChildren, revealChildren } from "./task-children-fixture";
import { advanceUntilVisible, installPausedClock } from "./clock";

async function arrange(page: Page, status = "RUNNING") {
  await installPausedClock(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  await mockApi(page);
  const children = Array.from({ length: 150 }, (_, index) => ({
    ...tasks[0],
    taskIndex: 12000 + index,
    shardIndex: index,
    subtaskNumber: index + 1,
    parentTaskId: 989,
    workerNode: "10.0.1.12",
    status,
  }));
  const parent = {
    ...tasks[0],
    taskIndex: 989,
    ossTaskGroup: true,
    status: "RUNNING",
    scanComplete: false,
    childTaskIds: children.map((child) => child.taskIndex),
  };
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { tasks: [parent], totalTasks: 1 } }),
  );
  const reads: string[] = [];
  await mockTaskChildren(
    page,
    () => ({ parentTaskId: 989, items: children }),
    true,
  );
  page.on("request", (request) => {
    if (new URL(request.url()).pathname === "/api/master/tasks/989/children")
      reads.push(request.url());
  });
  await page.goto("/tasks?taskId=989");
  await advanceUntilVisible(
    page,
    page.getByRole("heading", { name: "迁移任务 #989", exact: true }),
  );
  await revealChildren(page);
  await expect(page.locator("[data-child-id]")).toHaveCount(25);
  return { children, reads };
}

async function alignListTop(page: Page) {
  await page.getByTestId("task-children-region").evaluate((region) => {
    const drawer = region.closest<HTMLElement>(".MuiDrawer-paper")!;
    drawer.scrollTop +=
      region.getBoundingClientRect().top -
      drawer.getBoundingClientRect().top -
      8;
  });
  await page.clock.runFor(100);
}

const scrollTop = (page: Page) =>
  page.getByRole("dialog").evaluate((drawer) => drawer.scrollTop);
const row = (page: Page, id: number) =>
  page.locator(`[data-child-id="989-${id}"]`);
const rowTop = (page: Page, id: number) =>
  row(page, id).evaluate((element) => element.getBoundingClientRect().top);

test("completing the first visible child does not drag the reader to its history position", async ({
  page,
}) => {
  const { children } = await arrange(page);
  await alignListTop(page);
  const before = await scrollTop(page);
  children[0].status = "COMPLETED";
  await page.clock.runFor(1200);
  await expect(
    row(page, 1).getByText("已完成", { exact: true }),
  ).toBeAttached();
  expect(Math.abs((await scrollTop(page)) - before)).toBeLessThanOrEqual(2);
  await expect(row(page, 2)).toBeInViewport();
  await expect(row(page, 1)).not.toBeInViewport();
});

test("a middle-list reader stays on the same neighboring child when the leading visible child completes", async ({
  page,
}) => {
  const { children } = await arrange(page);
  await row(page, 8).evaluate((element) => {
    const drawer = element.closest<HTMLElement>(".MuiDrawer-paper")!;
    drawer.scrollTop +=
      element.getBoundingClientRect().top -
      drawer.getBoundingClientRect().top -
      8;
  });
  await page.clock.runFor(100);
  const neighborTop = await rowTop(page, 9);
  children[7].status = "COMPLETED";
  await page.clock.runFor(1200);
  await expect(
    row(page, 8).getByText("已完成", { exact: true }),
  ).toBeAttached();
  await expect(row(page, 9)).toBeInViewport();
  expect(Math.abs((await rowTop(page, 9)) - neighborTop)).toBeLessThanOrEqual(
    2,
  );
  await expect(row(page, 8)).not.toBeInViewport();
});

test("newly running children refresh at the head without scrolling away from it", async ({
  page,
}) => {
  const { children } = await arrange(page, "COMPLETED");
  await alignListTop(page);
  const before = await scrollTop(page);
  for (const index of [100, 101, 102]) {
    children[index].status = "RUNNING";
    await page.clock.runFor(1200);
    await expect(
      row(page, index + 1).getByText("执行中", { exact: true }),
    ).toBeAttached();
    expect(Math.abs((await scrollTop(page)) - before)).toBeLessThanOrEqual(2);
    await expect(row(page, 101)).toBeInViewport();
  }
});

for (const viewport of [
  { width: 1280, height: 800 },
  { width: 390, height: 844 },
]) {
  test(`quiet circular jump controls and bounded lazy loading at ${viewport.width}px`, async ({
    page,
  }, testInfo) => {
    await page.setViewportSize(viewport);
    const { reads } = await arrange(page);
    await alignListTop(page);
    const top = page.getByRole("button", { name: "滚动到顶部", exact: true });
    const bottom = page.getByRole("button", {
      name: "滚动到底部",
      exact: true,
    });
    await expect(top).not.toBeVisible();
    await expect(bottom).toBeVisible();
    const geometry = await bottom.evaluate((button) => {
      const rect = button.getBoundingClientRect();
      return {
        width: rect.width,
        height: rect.height,
        text: button.textContent,
      };
    });
    expect(geometry.width).toBeLessThanOrEqual(44);
    expect(geometry.width).toBe(geometry.height);
    expect(geometry.text).toBe("");
    await bottom.click();
    await page.clock.runFor(500);
    await expect(top).toBeVisible();
    await expect(row(page, 26)).toBeAttached();
    await page.clock.runFor(400);
    expect(await page.locator("[data-child-id]").count()).toBeLessThanOrEqual(
      50,
    );
    expect(
      reads.filter(
        (url) => Number(new URL(url).searchParams.get("offset")) >= 50,
      ),
    ).toHaveLength(0);
    await page.screenshot({
      path: testInfo.outputPath(`child-list-${viewport.width}.png`),
    });
    await top.click();
    await page.clock.runFor(500);
    await expect(row(page, 1)).toBeInViewport();
    await expect(top).not.toBeVisible();
    await expect(bottom).toBeVisible();
  });
}
