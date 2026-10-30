import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";
import { mockTaskChildren, revealChildren } from "./task-children-fixture";

async function arrange(page: Page, openChild = true) {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  const state = {
    parent: {
      ...tasks[0],
      taskIndex: 51,
      ossTaskGroup: true,
      shardCount: 2,
      generation: 1,
      status: "RUNNING",
      listObjectCount: 1,
      listObjectSize: 100 * 1024 ** 3,
      putObjectCount: 0,
      putObjectSize: 0,
      averageSpeedBytesPerSecond: 0 as number | null | undefined,
      currentSpeedBytesPerSecond: (181.4 * 1024 ** 2) as number | null,
    },
    progress: {
      supported: true,
      partial: false,
      generation: 1,
      observedAt: 0,
      transferredBytes: 25 * 1024 ** 3,
      stage: "TRANSFERRING",
      hasMore: false,
    },
    error: false,
    fileCount: 1,
    totalFiles: 1,
    reads: [] as string[],
  };
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { tasks: [state.parent] } }),
  );
  await page.route("**/api/master/tasks/51", (route) =>
    route.fulfill({ json: { data: state.parent } }),
  );
  await mockTaskChildren(page, () => ({
    parentTaskId: 51,
    items: [1, 2].map((number) => ({
      ...state.parent,
      parentTaskId: 51,
      childTaskId: `51-${number}`,
      shardId: `51-${number}`,
      subtaskNumber: number,
    })),
  }));
  await page.route("**/api/master/tasks/*/progress?*", (route) => {
    const url = new URL(route.request().url());
    state.reads.push(url.pathname + url.search);
    return route.fulfill({
      status: state.error ? 503 : 200,
      json: state.error
        ? { success: false, message: "unavailable" }
        : {
            success: true,
            data: {
              parentTaskId: 51,
              childNumber: url.searchParams.has("childNumber")
                ? Number(url.searchParams.get("childNumber"))
                : undefined,
              ...state.progress,
              observedAt: state.progress.observedAt || Date.now(),
              total: state.totalFiles,
              items: Array.from({ length: state.fileCount }, (_, index) => ({
                childTaskId: "51-1",
                subtaskNumber: 1,
                executionNodeLabel: "worker-one (10.0.1.12)",
                itemId: index === 0 ? "big" : `file-${index}`,
                sourceKey:
                  index === 0
                    ? "training/big.bin"
                    : `tiny/0002/735/2735${index}.bin`,
                destinationKey: "training/big.bin",
                size: state.parent.listObjectSize,
                transferredBytes: state.progress.transferredBytes,
                stage: state.progress.stage,
              })),
            },
          },
    });
  });
  await page.goto("/tasks?taskId=51");
  if (openChild) await openSubtask(page, 1);
  return state;
}

async function openSubtask(page: Page, number: number) {
  await revealChildren(page);
  await page
    .getByRole("button", { name: `查看子任务 51-${number}`, exact: true })
    .click();
  await expect(page.locator("[data-subtask-heading]")).toHaveText(
    `子任务 #51-${number}`,
  );
}

for (const openChild of [false, true]) {
  test(`${openChild ? "subtask" : "task"} keeps speed rows when a live sample is unavailable and recovers on refresh`, async ({
    page,
  }) => {
    const state = await arrange(page, openChild);
    const details = openChild
      ? page.locator("[data-subtask-heading]").locator("..")
      : page;
    const average = details
      .getByText("平均速度", { exact: true })
      .locator("..");
    const current = details
      .getByText("当前速度", { exact: true })
      .locator("..");
    await expect(average).toContainText("0 B/s");
    await expect(current).toContainText("181.4 MiB/s");
    state.parent.averageSpeedBytesPerSecond = 120 * 1024 ** 2;
    await expect(average).toContainText("120 MiB/s");

    // JSON omits an undefined field, as the API does for an expired sample.
    state.parent.averageSpeedBytesPerSecond = undefined;
    await expect(average).toBeVisible();
    await expect(average).toContainText("—");
    await expect(average).not.toContainText("0 B/s");
    await expect(current).toContainText("181.4 MiB/s");

    state.parent.averageSpeedBytesPerSecond = null;
    state.parent.currentSpeedBytesPerSecond = null;
    await expect(current).toContainText("—");
    await expect(average).toContainText("—");
    state.parent.averageSpeedBytesPerSecond = 125 * 1024 ** 2;
    state.parent.currentSpeedBytesPerSecond = 0;
    await expect(average).toContainText("125 MiB/s");
    await expect(current).toContainText("0 B/s");
  });
}

test("file progress is only requested inside the selected subtask and preserves large-file finalization", async ({
  page,
}, testInfo) => {
  const state = await arrange(page, false);
  const section = page.getByRole("region", { name: "文件进度", exact: true });
  await expect(
    page.getByRole("heading", { name: "迁移任务 #51", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("region", { name: "正在传输", exact: true }),
  ).toHaveCount(0);
  await expect(section).toHaveCount(0);
  expect(state.reads).toEqual([]);
  await openSubtask(page, 1);
  const progress = section.getByRole("progressbar", {
    name: "文件进度：training/big.bin",
  });
  await expect(progress).toHaveAttribute("aria-valuenow", "25");
  await expect(section).toContainText("25 GiB / 100 GiB");
  state.progress.transferredBytes = 60 * 1024 ** 3;
  state.parent.averageSpeedBytesPerSecond = 120 * 1024 ** 2;
  await expect(progress).toHaveAttribute("aria-valuenow", "60");
  await expect(section).toContainText("60 GiB / 100 GiB");
  await section.screenshot({
    path: testInfo.outputPath("subtask-file-progress.png"),
  });
  await expect(
    page
      .locator("[data-subtask-heading]")
      .locator("..")
      .getByText("已迁移", { exact: true })
      .locator(".."),
  ).toContainText("0 / 0 B");
  await expect(
    page
      .locator("[data-subtask-heading]")
      .locator("..")
      .getByText("平均速度", { exact: true })
      .locator(".."),
  ).toContainText("120 MiB/s");
  state.progress.transferredBytes = 100 * 1024 ** 3;
  state.progress.stage = "FINALIZING";
  await expect(progress).toHaveAttribute("aria-valuetext", "确认中");
  await expect(section).toContainText("100 GiB / 100 GiB");
  await expect(section).not.toContainText("已完成");
  expect(state.reads.every((url) => url.includes("childNumber=1"))).toBe(true);
  await page.getByRole("button", { name: "返回主任务", exact: true }).click();
  await expect(section).toHaveCount(0);
  const readsAfterReturn = state.reads.length;
  await page.waitForTimeout(1200); // Cross a polling interval after leaving the child.
  expect(state.reads).toHaveLength(readsAfterReturn);
  await openSubtask(page, 1);
  await expect(progress).toHaveAttribute("aria-valuetext", "确认中");
  await expect
    .poll(() =>
      state.reads.some(
        (url) => url.includes("childNumber=1") && url.includes("generation=1"),
      ),
    )
    .toBe(true);
  state.parent.status = "COMPLETED";
  await expect(section).toBeHidden();
});

test("failed and older progress responses keep the last values marked as stale", async ({
  page,
}) => {
  const state = await arrange(page);
  const section = page.getByRole("region", { name: "文件进度", exact: true });
  const progress = section.getByRole("progressbar", {
    name: "文件进度：training/big.bin",
  });
  await expect(progress).toHaveAttribute("aria-valuenow", "25");
  state.error = true;
  await expect(
    section.getByText("进度暂未更新", { exact: true }),
  ).toBeVisible();
  await expect(progress).toHaveAttribute("aria-valuenow", "25");
  state.error = false;
  state.progress.observedAt = Date.now() - 60_000;
  state.progress.transferredBytes = 0;
  await section.getByRole("button", { name: "刷新文件进度" }).click();
  await expect(progress).toHaveAttribute("aria-valuenow", "25");
  await expect(
    section.getByText("进度暂未更新", { exact: true }),
  ).toBeVisible();
  state.progress.observedAt = 0;
  state.progress.transferredBytes = 40 * 1024 ** 3;
  await expect(progress).toHaveAttribute("aria-valuenow", "40");
  await expect(section.getByText("进度暂未更新", { exact: true })).toBeHidden();
  state.progress.partial = true;
  await expect(
    section.getByText("进度暂未更新", { exact: true }),
  ).toBeVisible();
  await expect(progress).toHaveAttribute("aria-valuenow", "40");
});

test("unsupported agents and stale generations do not show misleading file progress", async ({
  page,
}) => {
  const state = await arrange(page);
  const section = page.getByRole("region", { name: "文件进度", exact: true });
  const progress = section.getByRole("progressbar", {
    name: "文件进度：training/big.bin",
  });
  await expect(progress).toHaveAttribute("aria-valuenow", "25");
  state.progress.supported = false;
  await expect(section.getByText("更新执行节点后可查看文件进度")).toBeVisible();
  await expect(progress).toBeHidden();
  state.progress.supported = true;
  state.parent.generation = 2;
  await expect
    .poll(() => state.reads.some((url) => url.includes("generation=2")))
    .toBe(true);
  await expect(progress).toBeHidden();
  await expect(
    section.getByText("进度暂未更新", { exact: true }),
  ).toBeVisible();
  state.progress.generation = 2;
  state.progress.transferredBytes = 5 * 1024 ** 3;
  await expect(progress).toHaveAttribute("aria-valuenow", "5");
});

test("switching children never carries over file progress while the next response is delayed", async ({
  page,
}) => {
  const state = await arrange(page);
  await expect(
    page.getByText("training/big.bin", { exact: true }),
  ).toBeVisible();
  let release!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route(
    /\/api\/master\/tasks\/51\/progress\?.*childNumber=2(?:&|$)/,
    async (route) => {
      await held;
      return route.fulfill({
        json: {
          success: true,
          data: {
            parentTaskId: 51,
            childNumber: 2,
            generation: 1,
            supported: true,
            partial: false,
            observedAt: Date.now(),
            items: [],
            total: 0,
            hasMore: false,
          },
        },
      });
    },
  );
  await page.getByRole("button", { name: "返回主任务", exact: true }).click();
  await openSubtask(page, 2);
  await expect(page.getByText("training/big.bin", { exact: true })).toHaveCount(
    0,
  );
  release();
  await expect(
    page.getByRole("region", { name: "文件进度", exact: true }),
  ).toBeHidden();
  expect(state.reads.every((url) => url.includes("childNumber=1"))).toBe(true);
});
