import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

const ids = Array.from({ length: 130 }, (_, index) => 130 - index);

async function recycleFixture(
  page: Page,
  removed: Set<number>,
  fromTasks = false,
) {
  await mockApi(page);
  await page.route("**/api/master/deleted-tasks", (route) => {
    const records = ids
      .filter((id) => !removed.has(id))
      .map((taskIndex) => ({
        taskIndex,
        sourceBucket: "source",
        destBucket: "dest",
        status: "STOPPED",
      }));
    return route.fulfill({
      json: {
        success: true,
        deletedTasks: {
          "10.0.1.12": records,
          // Historical responses can contain one parent on multiple nodes.
          "10.0.1.13": records.slice(0, 10),
        },
        queriedWorkers: ["10.0.1.12", "10.0.1.13"],
        totalCount: records.length + Math.min(10, records.length),
        complete: true,
      },
    });
  });
  if (fromTasks) {
    await page.goto("/tasks");
    await page.getByRole("link", { name: "运维工具", exact: true }).click();
    await page.getByRole("tab", { name: "任务回收站", exact: true }).click();
  } else await page.goto("/tools?tab=recycle");
  await page
    .getByRole("checkbox", { name: "选择全部已删除任务", exact: true })
    .check();
  await expect(
    page.getByText("已选择 130 个任务 ID", { exact: false }),
  ).toBeVisible();
  await page.getByRole("button", { name: "永久删除", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("永久删除 130 个任务？");
  await expect(page.getByRole("dialog")).not.toContainText("#1,");
}

test("bulk recycle deletion deduplicates parents, reports progress and keeps only failed IDs selected", async ({
  page,
}) => {
  const removed = new Set<number>();
  const submitted: number[][] = [];
  let releaseSecond!: () => void;
  const secondGate = new Promise<void>((resolve) => {
    releaseSecond = resolve;
  });
  await recycleFixture(page, removed);
  await page.route("**/api/master/permanent-delete", async (route) => {
    const batch = route.request().postDataJSON().taskIds as number[];
    submitted.push(batch);
    if (submitted.length === 2) await secondGate;
    const details = batch.map((taskId) => {
      const success = taskId !== 130;
      if (success) removed.add(taskId);
      return {
        taskId,
        success,
        accepted: success,
        message: success ? undefined : "Agent unavailable",
      };
    });
    return route.fulfill({
      json: {
        success: true,
        accepted: details.filter((item) => item.success).length,
        failed: details.filter((item) => !item.success).length,
        partial: details.some((item) => !item.success),
        details,
      },
    });
  });
  const confirm = page.getByRole("button", {
    name: "确认永久删除",
    exact: true,
  });
  await confirm.dispatchEvent("click");
  await confirm.dispatchEvent("click");
  await expect(page.getByRole("dialog")).toContainText("已处理 64 / 130");
  await expect(confirm).toBeDisabled();
  expect(submitted).toHaveLength(2);
  releaseSecond();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(
    page.getByText("已选择 1 个任务 ID", { exact: false }),
  ).toBeVisible();
  await expect(
    page.getByText("已处理 130 / 130", { exact: true }),
  ).toBeVisible();
  expect(submitted.map((batch) => batch.length)).toEqual([64, 64, 2]);
  expect(submitted.flat()).toEqual(ids);
  expect(new Set(submitted.flat()).size).toBe(130);
  await page.getByRole("button", { name: "查看失败详情", exact: true }).click();
  await expect(
    page.getByText("#130: Agent unavailable", { exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "永久删除", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("永久删除 1 个任务？");
  await page.getByRole("button", { name: "取消", exact: true }).click();
});

test("an unknown recycle response stops later batches and does not replay the request", async ({
  page,
}) => {
  const removed = new Set<number>();
  const submitted: number[][] = [];
  await recycleFixture(page, removed);
  await page.route("**/api/master/permanent-delete", async (route) => {
    const batch = route.request().postDataJSON().taskIds as number[];
    submitted.push(batch);
    if (submitted.length === 2) return route.abort("connectionreset");
    batch.forEach((id) => removed.add(id));
    return route.fulfill({
      json: {
        success: true,
        accepted: batch.length,
        failed: 0,
        details: batch.map((taskId) => ({
          taskId,
          success: true,
          accepted: true,
        })),
      },
    });
  });
  await page.getByRole("button", { name: "确认永久删除", exact: true }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(
    page.getByText("已选择 66 个任务 ID", { exact: false }),
  ).toBeVisible();
  await expect(page.getByRole("status")).toContainText("待确认 64，未提交 2");
  expect(submitted.map((batch) => batch.length)).toEqual([64, 64]);
  expect(submitted.flat()).toEqual(ids.slice(0, 128));
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(
    page.getByText("已选择 66 个任务 ID", { exact: false }),
  ).toBeVisible();
  expect(submitted).toHaveLength(2);
});

test("confirmed recycle deletion continues after SPA navigation and remains guarded on return", async ({
  page,
}) => {
  const removed = new Set<number>();
  const submitted: number[][] = [];
  let releaseSecond!: () => void;
  const secondGate = new Promise<void>((resolve) => {
    releaseSecond = resolve;
  });
  await recycleFixture(page, removed, true);
  await page.route("**/api/master/permanent-delete", async (route) => {
    const batch = route.request().postDataJSON().taskIds as number[];
    submitted.push(batch);
    if (submitted.length === 2) await secondGate;
    batch.forEach((id) => removed.add(id));
    return route.fulfill({
      json: {
        success: true,
        accepted: batch.length,
        failed: 0,
        details: batch.map((taskId) => ({
          taskId,
          success: true,
          accepted: true,
        })),
      },
    });
  });
  await page.getByRole("button", { name: "确认永久删除", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("已处理 64 / 130");
  // The previous entry was a React Router navigation, not a document reload.
  await page.goBack();
  await expect(
    page.getByRole("heading", { name: "迁移任务", exact: true }),
  ).toBeVisible();
  await page.goForward();
  await expect(
    page.getByRole("checkbox", { name: "选择全部已删除任务", exact: true }),
  ).toBeDisabled();
  await page.goBack();
  await expect(
    page.getByRole("heading", { name: "迁移任务", exact: true }),
  ).toBeVisible();
  releaseSecond();
  await expect.poll(() => submitted.flat().length).toBe(130);
  expect(submitted.map((batch) => batch.length)).toEqual([64, 64, 2]);
  expect(submitted.flat()).toEqual(ids);
  await expect(
    page.getByText("永久删除请求已提交，正在刷新回收站。", { exact: true }),
  ).toBeVisible();
});
