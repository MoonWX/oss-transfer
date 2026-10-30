import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";
import { mockTaskChildren } from "./task-children-fixture";

async function arrange(page: Page, expanded = true) {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  const state = {
    parent: {
      ...tasks[0],
      taskIndex: 51,
      generation: 1,
      status: "RUNNING",
      listObjectCount: 1,
      listObjectSize: 100 * 1024 ** 3,
      putObjectCount: 0,
      putObjectSize: 0,
      averageSpeedBytesPerSecond: 0,
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
    items: [
      {
        ...state.parent,
        parentTaskId: 51,
        childTaskId: "51-1",
        shardId: "51-1",
        subtaskNumber: 1,
      },
    ],
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
  if (expanded)
    await page
      .getByRole("button", { name: "文件传输详情", exact: true })
      .click();
  return state;
}

test("a large unfinished file advances before completed counters and stays finalizing at 100 percent", async ({
  page,
}) => {
  const state = await arrange(page);
  const section = page.getByRole("region", { name: "正在传输", exact: true });
  const progress = section.getByRole("progressbar", {
    name: "文件进度：training/big.bin",
  });
  await expect(progress).toHaveAttribute("aria-valuenow", "25");
  await expect(section).toContainText("25 GiB / 100 GiB");
  state.progress.transferredBytes = 60 * 1024 ** 3;
  state.parent.averageSpeedBytesPerSecond = 120 * 1024 ** 2;
  await expect(progress).toHaveAttribute("aria-valuenow", "60");
  await expect(section).toContainText("60 GiB / 100 GiB");
  await expect(
    page.getByText("已迁移", { exact: true }).locator(".."),
  ).toContainText("0 / 0 B");
  await expect(
    page.getByText("平均速度", { exact: true }).locator(".."),
  ).toContainText("120 MiB/s");
  state.progress.transferredBytes = 100 * 1024 ** 3;
  state.progress.stage = "FINALIZING";
  await expect(progress).toHaveAttribute("aria-valuetext", "确认中");
  await expect(section).toContainText("100 GiB / 100 GiB");
  await expect(section).not.toContainText("已完成");
  await section
    .getByRole("button", { name: "子任务 #51-1", exact: true })
    .click();
  await expect(page.locator("[data-subtask-heading]")).toHaveText(
    "子任务 #51-1",
  );
  await page.getByRole("button", { name: "文件传输详情", exact: true }).click();
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
  const section = page.getByRole("region", { name: "正在传输", exact: true });
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
  const section = page.getByRole("region", { name: "正在传输", exact: true });
  const progress = section.getByRole("progressbar", {
    name: "文件进度：training/big.bin",
  });
  await expect(progress).toHaveAttribute("aria-valuenow", "25");
  state.progress.supported = false;
  await expect(section.getByText("更新执行节点后可查看文件进度")).toBeVisible();
  await expect(
    section.getByRole("button", { name: "文件传输详情", exact: true }),
  ).toBeDisabled();
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
  await page.getByRole("button", { name: "文件传输详情", exact: true }).click();
  await expect(progress).toHaveAttribute("aria-valuenow", "5");
});

test("many small files stay collapsed and expand into a bounded list", async ({
  page,
}, testInfo) => {
  const state = await arrange(page, false);
  state.fileCount = 20;
  state.totalFiles = 37;
  state.parent.listObjectSize = 6 * 1024;
  state.progress.transferredBytes = 0;
  state.progress.hasMore = true;
  const section = page.getByRole("region", { name: "正在传输", exact: true });
  const toggle = section.getByRole("button", {
    name: "文件传输详情",
    exact: true,
  });
  await expect(section).toContainText("37 个文件");
  await expect(toggle).toHaveAttribute("aria-expanded", "false");
  await expect(section.locator("[data-file-progress]")).toHaveCount(0);
  await expect
    .poll(async () => (await section.boundingBox())!.height)
    .toBeLessThan(60);
  await section.screenshot({ path: testInfo.outputPath("collapsed.png") });
  await toggle.click();
  await expect(section.locator("[data-file-progress]")).toHaveCount(20);
  await expect(section.getByRole("progressbar").first()).toHaveAttribute(
    "aria-valuenow",
    "0",
  );
  await expect
    .poll(async () => (await section.boundingBox())!.height)
    .toBeLessThan(390);
  await expect(
    section.getByRole("button", { name: "在子任务中查看其他文件" }),
  ).toBeEnabled();
  await section.screenshot({ path: testInfo.outputPath("expanded.png") });
  await page.setViewportSize({ width: 375, height: 812 });
  await expect
    .poll(async () =>
      section.evaluate((element) => element.scrollWidth <= element.clientWidth),
    )
    .toBe(true);
  await section.screenshot({ path: testInfo.outputPath("mobile.png") });
  await toggle.click();
  await expect(section.locator("[data-file-progress]")).toHaveCount(0);
});

test("changing tasks does not carry over the previous file while the next response is delayed", async ({
  page,
}) => {
  const state = await arrange(page);
  await expect(
    page.getByText("training/big.bin", { exact: true }),
  ).toBeVisible();
  const second = { ...state.parent, taskIndex: 52 };
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { tasks: [state.parent, second] } }),
  );
  await page.route("**/api/master/tasks/52", (route) =>
    route.fulfill({ json: { data: second } }),
  );
  let release!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route("**/api/master/tasks/52/progress?*", async (route) => {
    await held;
    return route.fulfill({
      json: {
        success: true,
        data: {
          parentTaskId: 52,
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
  });
  await page.getByRole("button", { name: "关闭详情", exact: true }).click();
  await page.getByRole("button", { name: "#52", exact: true }).click();
  await expect(
    page.getByText("training/big.bin", { exact: true }),
  ).toBeHidden();
  release();
  await expect(
    page.getByRole("region", { name: "正在传输", exact: true }),
  ).toBeHidden();
});
