import { expect, test } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

for (const operation of ["stop", "resume"] as const) {
  for (const limit of [null, 100]) {
    test(`${operation} with ${limit ?? "no"} task limit waits for policy synchronization without a retry warning`, async ({
      page,
    }) => {
      await mockApi(page);
      await page.addInitScript(() =>
        localStorage.setItem("oss-transfer-refresh-seconds", "1"),
      );
      const task = {
        ...tasks[2],
        failedObjectCount: 0,
        status: operation === "stop" ? "RUNNING" : "STOPPED",
      };
      let pending = false;
      let pendingReads = 0;
      let limitWrites = 0;
      await page.route("**/api/master/status", (route) =>
        route.fulfill({ json: { tasks: [task] } }),
      );
      await page.route("**/api/master/tasks/1046/bandwidth-limit", (route) => {
        if (route.request().method() !== "GET") limitWrites++;
        if (pending) pendingReads++;
        return route.fulfill({
          json: {
            success: true,
            data: {
              limitMbps: limit,
              applied: !pending,
              pending,
              sharedTaskCount: 1,
              canEdit: true,
              nodes: ["10.0.1.12", "10.0.1.13"].map((address) => ({
                address,
                applied: !pending,
                pending,
              })),
            },
          },
        });
      });
      await page.route(
        `**/api/master/local-transfers/${operation}`,
        (route) => {
          pending = true;
          task.status = operation === "stop" ? "STOPPING" : "RESUMING";
          if (operation === "resume") task.generation++;
          return route.fulfill({
            json: {
              success: true,
              accepted: 1,
              details: [
                {
                  taskId: task.taskIndex,
                  success: true,
                  generation: task.generation,
                  status: task.status,
                },
              ],
            },
          });
        },
      );
      await page.goto("/?taskId=1046");
      await page
        .getByRole("button", {
          name: operation === "stop" ? "停止任务" : "继续传输",
          exact: true,
        })
        .click();
      await page
        .locator(".MuiDialog-root [role=dialog]")
        .getByRole("button", {
          name: operation === "stop" ? "确认停止" : "确认继续传输",
          exact: true,
        })
        .click();
      await expect.poll(() => pendingReads).toBeGreaterThanOrEqual(3);
      const drawer = page.locator(".MuiDrawer-paper");
      await expect(
        drawer.getByText("部分节点未生效", { exact: true }),
      ).toHaveCount(0);
      await expect(
        drawer.getByRole("button", { name: "重试", exact: true }),
      ).toHaveCount(0);
      expect(limitWrites).toBe(0);
      pending = false;
      task.status = operation === "stop" ? "STOPPED" : "RUNNING";
      await expect(
        drawer.getByText(operation === "stop" ? "已停止" : "执行中", {
          exact: true,
        }),
      ).toBeVisible();
      await expect(
        drawer.getByText("部分节点未生效", { exact: true }),
      ).toHaveCount(0);
      expect(limitWrites).toBe(0);
    });
  }
}

test("an offline node is still warned while another node synchronizes, and settled mismatches remain visible", async ({
  page,
}) => {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  let syncing = true;
  await page.route("**/api/master/tasks/1048/bandwidth-limit", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          limitMbps: 100,
          applied: false,
          pending: false,
          sharedTaskCount: 1,
          canEdit: true,
          nodes: [
            { address: "10.0.1.12", applied: false, pending: syncing },
            {
              address: "10.0.1.13",
              applied: false,
              pending: false,
              message: "执行节点未连接",
            },
          ],
        },
      },
    }),
  );
  await page.goto("/tasks?taskId=1048");
  const warning = page
    .locator(".MuiDrawer-paper")
    .getByRole("alert")
    .filter({ hasText: "部分节点未生效" });
  await expect(warning).toContainText("10.0.1.13");
  await expect(warning).toContainText("执行节点未连接");
  await expect(warning).not.toContainText("10.0.1.12");
  syncing = false;
  await expect(warning).toContainText("10.0.1.12");
  await expect(
    warning.getByRole("button", { name: "重试", exact: true }),
  ).toBeEnabled();
});

test("the limit editor keeps polling pending synchronization without automatically writing settings", async ({
  page,
}) => {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  let pending = true;
  let reads = 0;
  let writes = 0;
  await page.route("**/api/master/tasks/1048/bandwidth-limit", (route) => {
    if (route.request().method() === "GET") reads++;
    else writes++;
    return route.fulfill({
      json: {
        success: true,
        data: {
          limitMbps: 100,
          applied: !pending,
          pending,
          sharedTaskCount: 1,
          canEdit: true,
          nodes: [{ address: "10.0.1.12", applied: !pending, pending }],
        },
      },
    });
  });
  await page.goto("/tasks?taskId=1048");
  await page.getByRole("button", { name: "调整", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "任务 #1048 限速" });
  await expect(dialog.getByRole("status")).toHaveText("正在同步限速设置…");
  await expect(dialog.getByRole("alert")).toHaveCount(0);
  const before = reads;
  await expect.poll(() => reads).toBeGreaterThan(before);
  pending = false;
  await expect(dialog.getByRole("status")).toHaveCount(0);
  await expect(
    dialog.getByRole("button", { name: "应用", exact: true }),
  ).toBeEnabled();
  expect(writes).toBe(0);
});

test("pending synchronization polling pauses while the user saves new limits", async ({
  page,
}) => {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  let reads = 0;
  let writes = 0;
  let finishSave!: () => void;
  const saving = new Promise<void>((resolve) => (finishSave = resolve));
  let current = {
    limitMbps: 100,
    requestLimits: { scope: "CLUSTER", putQps: 30 },
    applied: false,
    pending: true,
    sharedTaskCount: 1,
    canEdit: true,
    nodes: [{ address: "10.0.1.12", applied: false, pending: true }],
  };
  await page.route(
    "**/api/master/tasks/1048/bandwidth-limit",
    async (route) => {
      if (route.request().method() === "PUT") {
        writes++;
        await saving;
        current = {
          ...current,
          ...route.request().postDataJSON(),
          applied: true,
          pending: false,
          nodes: [{ address: "10.0.1.12", applied: true, pending: false }],
        };
      } else reads++;
      await route.fulfill({ json: { success: true, data: current } });
    },
  );
  await page.goto("/tasks?taskId=1048");
  await page.getByRole("button", { name: "调整", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "任务 #1048 限速" });
  await dialog.getByLabel("PUT QPS").fill("60");
  await dialog.getByRole("button", { name: "应用", exact: true }).click();
  await expect.poll(() => writes).toBe(1);
  const before = reads;
  // Hold the PUT across two refresh ticks; stale GETs must not replace its result.
  await page.waitForTimeout(2200);
  expect(reads).toBe(before);
  finishSave();
  await expect(dialog).toHaveCount(0);
  await page.getByRole("button", { name: "调整", exact: true }).click();
  await expect(dialog.getByLabel("PUT QPS")).toHaveValue("60");
  expect(writes).toBe(1);
});
