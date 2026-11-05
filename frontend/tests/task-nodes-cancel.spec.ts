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
for (const [address, state] of [
  ["10.0.1.13", "平滑移出中"],
  ["10.0.1.16", "立即移出中"],
] as const) {
  test(`local transfers cancel a removal without a binding (${address})`, async ({
    page,
  }) => {
    const posts = await setup(page);
    await page.goto("/tasks?taskId=1048");
    await page.getByRole("button", { name: "编辑节点", exact: true }).click();
    const box = page.getByRole("dialog", { name: "任务 #1048 执行节点" });
    const node = box.getByTestId(`task-node-${address}`);
    await expect(node.getByText(state, { exact: true })).toBeVisible();
    await box
      .getByRole("button", { name: new RegExp(`^保留 .*${address}`) })
      .click();
    await expect(node).toHaveAttribute("data-marked", "add");
    await expect(node).toContainText("将保留");
    // Keeping a leaving node is not a removal: no mode choice.
    await expect(box.getByRole("radio")).toHaveCount(0);
    await expect(
      box.getByRole("button", { name: "预检", exact: true }),
    ).toHaveCount(0);
    await expect(
      box.getByRole("textbox", {
        name: new RegExp(`${address} 的本地绝对路径`),
      }),
    ).toHaveCount(0);
    await expect(box.getByTestId("task-nodes-footer")).toHaveText(
      "待提交：保留 1 个节点",
    );
    await box.getByRole("button", { name: "预览变更", exact: true }).click();
    await expect(box.getByTestId("task-nodes-summary")).toContainText(
      "保留（取消移出）",
    );
    await box
      .getByRole("button", { name: "保留 1 个节点", exact: true })
      .click();
    await expect.poll(() => posts.length).toBe(1);
    expect(posts[0].add).toEqual([address]);
    expect(posts[0]).not.toHaveProperty("bindings");
    expect(posts[0]).not.toHaveProperty("dryRun");
  });
}
