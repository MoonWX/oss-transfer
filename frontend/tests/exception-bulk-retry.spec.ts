import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

const task = (taskIndex: number, status = "PARTIAL_FAILED") => ({
  taskIndex,
  status,
  generation: taskIndex + 10,
  sourceBucket: `source-${taskIndex}`,
  destBucket: `target-${taskIndex}`,
  direction: "OSS_TO_OSS",
  failedObjectCount: 3,
});
async function openBulk(page: Page) {
  await page.goto("/exceptions");
  await page.getByRole("button", { name: "批量重试", exact: true }).click();
  return page.getByRole("dialog", { name: "批量重试失败任务", exact: true });
}
async function activeSnapshots(page: Page) {
  await page.route(/\/api\/master\/tasks\/\d+$/, (route) =>
    route.fulfill({ json: { data: { status: "RUNNING", generation: 99999 } } }),
  );
}
function accepted(ids: number[]) {
  return {
    success: true,
    accepted: ids.length,
    ignored: 0,
    failed: 0,
    details: ids.map((id) => ({
      taskId: id,
      generation: id + 11,
      status: "RESUMING",
      success: true,
      accepted: true,
    })),
  };
}

test("bulk retry submits only selected failed parents and prevents duplicate clicks", async ({
  page,
}) => {
  await mockApi(page);
  await activeSnapshots(page);
  await page.route("**/api/master/status?**", (route) =>
    route.fulfill({
      json: {
        tasks: [
          task(901, "FAILED"),
          task(902),
          task(903, "STOPPED"),
          task(904, "RUNNING"),
          task(905, "COMPLETED"),
        ],
        hasMore: false,
        pageComplete: true,
      },
    }),
  );
  const posts: any[] = [];
  let reply: (() => void) | undefined;
  await page.route("**/api/master/retry", async (route) => {
    posts.push(route.request().postDataJSON());
    await new Promise<void>((resolve) => {
      reply = resolve;
    });
    await route.fulfill({ json: accepted(posts[0].taskIds) });
  });
  const dialog = await openBulk(page);
  await expect(
    dialog.getByRole("checkbox", { name: "选择任务 #901", exact: true }),
  ).toBeVisible();
  await expect(dialog.getByRole("checkbox")).toHaveCount(3);
  await expect(dialog.getByText("#903", { exact: true })).toHaveCount(0);
  await dialog
    .getByRole("checkbox", { name: "选择任务 #901", exact: true })
    .check();
  await dialog
    .getByRole("button", { name: "重试所选（1）", exact: true })
    .dblclick();
  await expect.poll(() => posts.length).toBe(1);
  expect(posts[0]).toEqual({
    taskIds: [901],
    expectedGenerations: { "901": 911 },
  });
  await expect(
    dialog.getByRole("button", { name: "提交中…", exact: true }),
  ).toBeDisabled();
  reply!();
  await expect(
    dialog.getByText("已受理 1，忽略 0，失败 0", { exact: true }),
  ).toBeVisible();
  await expect(
    dialog.getByRole("checkbox", { name: "选择任务 #901", exact: true }),
  ).toBeDisabled();
  await expect(
    dialog.getByRole("checkbox", { name: "选择任务 #902", exact: true }),
  ).toBeEnabled();
  expect(posts).toHaveLength(1);
});

test("retry all follows empty cursor pages and batches more than 128 eligible tasks without including active tasks", async ({
  page,
}) => {
  await mockApi(page);
  await activeSnapshots(page);
  const first = Array.from({ length: 70 }, (_, i) => task(2001 + i));
  const last = Array.from({ length: 60 }, (_, i) => task(i + 1, "FAILED"));
  const cursors: string[] = [];
  await page.route("**/api/master/status?**", (route) => {
    const cursor =
      new URL(route.request().url()).searchParams.get("beforeId") || "first";
    cursors.push(cursor);
    const result =
      cursor === "first"
        ? {
            tasks: [...first, task(3000, "RUNNING"), task(3001, "STOPPED")],
            hasMore: true,
            nextBeforeId: 2001,
          }
        : cursor === "2001"
          ? { tasks: [], hasMore: true, nextBeforeId: 1000 }
          : { tasks: [...last, first[0]], hasMore: false };
    return route.fulfill({ json: { ...result, pageComplete: true } });
  });
  const posts: any[] = [];
  await page.route("**/api/master/retry", (route) => {
    const body = route.request().postDataJSON();
    posts.push(body);
    return route.fulfill({ json: accepted(body.taskIds) });
  });
  const dialog = await openBulk(page);
  await dialog
    .getByRole("button", { name: "重试全部（130）", exact: true })
    .click();
  await expect(
    dialog.getByText("已受理 130，忽略 0，失败 0", { exact: true }),
  ).toBeVisible();
  expect(cursors).toContain("2001");
  expect(cursors).toContain("1000");
  expect(posts.map((post) => post.taskIds.length)).toEqual([64, 64, 2]);
  expect(posts.flatMap((post) => post.taskIds).sort((a, b) => a - b)).toEqual(
    [...first, ...last].map((row) => row.taskIndex).sort((a, b) => a - b),
  );
  for (const post of posts)
    expect(post.expectedGenerations).toEqual(
      Object.fromEntries(post.taskIds.map((id: number) => [id, id + 10])),
    );
});

test("a failed later catalog page blocks partial bulk submission", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/status?**", (route) => {
    const cursor = new URL(route.request().url()).searchParams.get("beforeId");
    return cursor
      ? route.fulfill({
          status: 503,
          json: { success: false, message: "Task history unavailable" },
        })
      : route.fulfill({
          json: { tasks: [task(11)], hasMore: true, nextBeforeId: 11 },
        });
  });
  let posts = 0;
  await page.route("**/api/master/retry", (route) => {
    posts++;
    return route.fulfill({ json: accepted([]) });
  });
  const dialog = await openBulk(page);
  await expect(
    dialog.getByText("Task history unavailable", { exact: true }),
  ).toBeVisible();
  await expect(dialog.getByRole("button", { name: /重试全部/ })).toBeDisabled();
  await expect(dialog.getByRole("button", { name: /重试所选/ })).toBeDisabled();
  expect(posts).toBe(0);
});

test("an uncertain batch is never replayed and unsubmitted tasks remain available", async ({
  page,
}) => {
  await mockApi(page);
  await activeSnapshots(page);
  await page.route("**/api/master/status?**", (route) =>
    route.fulfill({
      json: {
        tasks: Array.from({ length: 129 }, (_, i) => task(i + 1)),
        hasMore: false,
      },
    }),
  );
  let posts = 0;
  await page.route("**/api/master/retry", (route) => {
    posts++;
    return route.abort("failed");
  });
  const dialog = await openBulk(page);
  await dialog
    .getByRole("button", { name: "重试全部（129）", exact: true })
    .click();
  await expect(
    dialog.getByText("待确认 64，未提交 65", { exact: true }),
  ).toBeVisible();
  await expect(
    dialog.getByRole("button", { name: "重试全部（65）", exact: true }),
  ).toBeEnabled();
  await expect(
    dialog.getByRole("checkbox", { name: "选择任务 #129", exact: true }),
  ).toBeDisabled();
  expect(posts).toBe(1);
  await dialog.getByRole("button", { name: "关闭", exact: true }).click();
  await page.getByRole("button", { name: "批量重试", exact: true }).click();
  const reopened = page.getByRole("dialog", {
    name: "批量重试失败任务",
    exact: true,
  });
  await expect(
    reopened.getByRole("button", { name: "重试全部（65）", exact: true }),
  ).toBeEnabled();
  await expect(
    reopened.getByRole("checkbox", { name: "选择任务 #129", exact: true }),
  ).toBeDisabled();
  expect(posts).toBe(1);
});

test("per-task rejection shows its reason and preserves accepted tasks", async ({
  page,
}) => {
  await mockApi(page);
  await activeSnapshots(page);
  await page.route("**/api/master/status?**", (route) =>
    route.fulfill({ json: { tasks: [task(1), task(2)], hasMore: false } }),
  );
  await page.route("**/api/master/retry", (route) =>
    route.fulfill({
      json: {
        success: true,
        partial: true,
        accepted: 1,
        ignored: 0,
        failed: 1,
        details: [
          { taskId: 1, accepted: true, success: true, generation: 12 },
          { taskId: 2, success: false, message: "Task generation changed" },
        ],
      },
    }),
  );
  const dialog = await openBulk(page);
  await dialog
    .getByRole("button", { name: "重试全部（2）", exact: true })
    .click();
  await expect(
    dialog.getByText("已受理 1，忽略 0，失败 1", { exact: true }),
  ).toBeVisible();
  await expect(
    dialog.getByText("#2: Task generation changed", { exact: true }),
  ).toBeVisible();
  await expect(
    dialog.getByRole("checkbox", { name: "选择任务 #1", exact: true }),
  ).toBeDisabled();
});
