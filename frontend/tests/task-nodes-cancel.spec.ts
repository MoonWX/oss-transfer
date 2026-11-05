import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

type Json = Record<string, any>;

const member = (address: string, extra: Json = {}) => ({
  id: `node-${address}`,
  address,
  state: "ACTIVE",
  drainMode: "",
  scanner: false,
  online: true,
  pendingChildren: 0,
  localPath: "/mnt/share",
  share: { bandwidthBytesPerSecond: 0, listQps: 0, headQps: 0, putQps: 0 },
  ...extra,
});

const view = (): Json => ({
  taskId: 1048,
  generation: 1,
  nodeRevision: 4,
  status: "RUNNING",
  direction: "OSS_TO_LOCAL",
  localTransfer: true,
  editable: true,
  reason: "",
  nowSupported: true,
  nowReason: "",
  pending: true,
  bandwidthLimitMbps: null,
  nodes: [
    member("10.0.1.12", { scanner: true }),
    member("10.0.1.13", { state: "DRAINING", drainMode: "drain" }),
    member("10.0.1.16", { state: "REMOVING", drainMode: "now" }),
  ],
  removedNodes: [],
  candidates: [],
});

async function setup(page: Page) {
  await mockApi(page);
  const rows = tasks.map((task) =>
    task.taskIndex === 1048
      ? {
          ...task,
          executionMembership: {
            nodeRevision: 4,
            pending: true,
            nodes: view().nodes,
            joining: [],
            draining: ["10.0.1.13"],
          },
        }
      : task,
  );
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: { success: true, totalTasks: rows.length, tasks: rows },
    }),
  );
  const posts: Json[] = [];
  await page.route("**/api/master/tasks/1048/nodes", async (route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({ json: { success: true, data: view() } });
      return;
    }
    posts.push(route.request().postDataJSON());
    await route.fulfill({
      json: {
        success: true,
        data: {
          accepted: true,
          dryRun: false,
          replayed: false,
          valid: true,
          nodeBindings: [],
          validationErrors: [],
          view: { ...view(), pending: false },
        },
      },
    });
  });
  return posts;
}

// Re-adding a leaving node keeps its binding: no path, no preflight.
for (const [address, label] of [
  ["10.0.1.13", "取消排空，保留"],
  ["10.0.1.16", "取消立即移除，保留"],
] as const) {
  test(`local transfers cancel a removal without a binding (${address})`, async ({
    page,
  }) => {
    const posts = await setup(page);
    await page.goto("/tasks?taskId=1048");
    await page.getByRole("button", { name: "编辑节点", exact: true }).click();
    const box = page.getByRole("dialog", { name: "任务 #1048 执行节点" });
    await box
      .getByRole("checkbox", { name: new RegExp(`${label} .*${address}`) })
      .check();
    await expect(
      box.getByRole("button", { name: "预检", exact: true }),
    ).toHaveCount(0);
    await expect(
      box.getByRole("textbox", {
        name: new RegExp(`${address} 的本地绝对路径`),
      }),
    ).toHaveCount(0);
    await box.getByRole("button", { name: "下一步", exact: true }).click();
    await box.getByRole("button", { name: "确认变更", exact: true }).click();
    await expect.poll(() => posts.length).toBe(1);
    expect(posts[0].add).toEqual([address]);
    expect(posts[0]).not.toHaveProperty("bindings");
    expect(posts[0]).not.toHaveProperty("dryRun");
  });
}
