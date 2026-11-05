import { expect, test, type Page, type Route } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";
import { advanceUntilVisible, installPausedClock } from "./clock";

type Json = Record<string, any>;

const member = (address: string, extra: Json = {}) => ({
  id: `node-${address}`,
  address,
  state: "ACTIVE",
  drainMode: "",
  scanner: false,
  online: true,
  pendingChildren: 0,
  share: { bandwidthBytesPerSecond: 0, listQps: 0, headQps: 0, putQps: 0 },
  ...extra,
});

function nodesView(extra: Json = {}): Json {
  return {
    taskId: 1048,
    generation: 1,
    nodeRevision: 3,
    status: "RUNNING",
    direction: "OSS_TO_OSS",
    localTransfer: false,
    editable: true,
    reason: "",
    nowSupported: true,
    nowReason: "",
    pending: false,
    bandwidthLimitMbps: null,
    nodes: [
      member("10.0.1.12", { scanner: true }),
      member("10.0.1.13", { pendingChildren: 2 }),
    ],
    removedNodes: [],
    candidates: [
      { id: "node-14", address: "10.0.1.14", eligible: true, reason: "" },
      {
        id: "node-15",
        address: "10.0.1.15",
        eligible: false,
        reason: "Agent 版本过旧",
      },
    ],
    ...extra,
  };
}

const membership = (nodes: Json[]) => ({
  nodeRevision: 3,
  pending: nodes.some((node) => node.state !== "ACTIVE"),
  nodes,
  joining: nodes.filter((n) => n.state === "JOINING").map((n) => n.address),
  draining: nodes.filter((n) => n.state === "DRAINING").map((n) => n.address),
});

async function setup(
  page: Page,
  options: {
    taskId?: number;
    taskExtra?: Json;
    view?: () => Json;
    post?: (body: Json, route: Route) => Promise<void> | void;
  } = {},
) {
  await mockApi(page);
  const taskId = options.taskId ?? 1048;
  const rows = tasks.map((task) =>
    task.taskIndex === taskId
      ? {
          ...task,
          executionMembership: membership([
            member("10.0.1.12", { scanner: true }),
            member("10.0.1.13"),
          ]),
          ...options.taskExtra,
        }
      : task,
  );
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: {
        success: true,
        totalTasks: rows.length,
        tasks: rows,
      },
    }),
  );
  const posts: Json[] = [];
  let gets = 0;
  await page.route(`**/api/master/tasks/${taskId}/nodes`, async (route) => {
    if (route.request().method() === "GET") {
      gets++;
      await route.fulfill({
        json: { success: true, data: (options.view ?? nodesView)() },
      });
      return;
    }
    const body = route.request().postDataJSON();
    posts.push(body);
    if (options.post) await options.post(body, route);
    else
      await route.fulfill({
        json: {
          success: true,
          data: {
            accepted: !body.dryRun,
            dryRun: !!body.dryRun,
            replayed: false,
            valid: true,
            nodeBindings: [],
            validationErrors: [],
            view: nodesView(),
          },
        },
      });
  });
  return { posts, gets: () => gets };
}

const dialog = (page: Page) =>
  page.getByRole("dialog", { name: "任务 #1048 执行节点" });

async function openDialog(page: Page) {
  await page.goto("/tasks?taskId=1048");
  await page.getByRole("button", { name: "编辑节点", exact: true }).click();
  await expect(dialog(page)).toBeVisible();
}

test("task detail shows membership states and shares and adds an Agent after confirmation", async ({
  page,
}) => {
  const { posts } = await setup(page, {
    taskExtra: {
      executionMembership: membership([
        member("10.0.1.12", {
          scanner: true,
          share: { bandwidthBytesPerSecond: 500000 },
        }),
        member("10.0.1.13", { state: "DRAINING", drainMode: "drain" }),
        member("10.0.1.14", { state: "JOINING", share: null }),
        member("10.0.1.16", { state: "REMOVING", drainMode: "now" }),
      ]),
    },
  });
  await page.goto("/tasks?taskId=1048");
  const row = page.getByTestId("task-membership-nodes");
  await expect(row.getByText("排空中", { exact: true })).toBeVisible();
  await expect(row.getByText("加入中", { exact: true })).toBeVisible();
  await expect(row.getByText("立即移除中", { exact: true })).toBeVisible();
  await expect(
    row.getByText("带宽份额 488.3 KiB/s", { exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "编辑节点", exact: true }).click();
  const box = dialog(page);
  await expect(box.getByText("扫描节点", { exact: true })).toBeVisible();
  await expect(box.getByText("未完成子任务 2", { exact: true })).toBeVisible();
  await expect(
    box.getByRole("checkbox", { name: /添加 .*10\.0\.1\.15/ }),
  ).toBeDisabled();
  await expect(box.getByText("Agent 版本过旧", { exact: true })).toBeVisible();
  const next = box.getByRole("button", { name: "下一步", exact: true });
  await expect(next).toBeDisabled();
  await box.getByRole("checkbox", { name: /添加 .*10\.0\.1\.14/ }).check();
  await next.click();
  await expect(box.getByTestId("task-nodes-summary")).toContainText(
    "添加：10.0.1.14",
  );
  await expect(box.getByTestId("task-nodes-summary")).not.toContainText("移除");
  await box.getByRole("button", { name: "确认变更", exact: true }).click();
  await expect(dialog(page)).toHaveCount(0);
  await expect(page.getByText("执行节点变更已提交。")).toBeVisible();
  expect(posts).toHaveLength(1);
  expect(posts[0]).toEqual({
    requestId: expect.stringMatching(/^[a-f0-9]{32}$/),
    add: ["10.0.1.14"],
    remove: [],
    expectedGeneration: 1,
    expectedNodeRevision: 3,
  });
});

for (const mode of ["drain", "now"] as const) {
  test(`removing a node submits mode ${mode}`, async ({ page }) => {
    const { posts } = await setup(page);
    await openDialog(page);
    const box = dialog(page);
    await expect(box.getByRole("radio")).toHaveCount(0);
    await box.getByRole("checkbox", { name: /移除 .*10\.0\.1\.13/ }).check();
    await expect(box.getByRole("radio", { name: /^排空后移除/ })).toBeChecked();
    if (mode === "now")
      await box.getByRole("radio", { name: /^立即移除/ }).check();
    await box.getByRole("button", { name: "下一步", exact: true }).click();
    const summary = box.getByTestId("task-nodes-summary");
    await expect(summary).toContainText("移除：agent (10.0.1.13)");
    await expect(summary).toContainText(
      mode === "now" ? "移除方式：立即移除" : "移除方式：排空后移除",
    );
    await box.getByRole("button", { name: "确认变更", exact: true }).click();
    await expect(dialog(page)).toHaveCount(0);
    expect(posts).toEqual([
      {
        requestId: expect.stringMatching(/^[a-f0-9]{32}$/),
        add: [],
        remove: ["10.0.1.13"],
        mode,
        expectedGeneration: 1,
        expectedNodeRevision: 3,
      },
    ]);
  });
}

test("remove now is disabled with the server reason and removing every node is blocked", async ({
  page,
}) => {
  await setup(page, {
    view: () =>
      nodesView({ nowSupported: false, nowReason: "单节点扫描任务不支持" }),
  });
  await openDialog(page);
  const box = dialog(page);
  await box.getByRole("checkbox", { name: /移除 .*10\.0\.1\.12/ }).check();
  await expect(box.getByRole("radio", { name: /^立即移除/ })).toBeDisabled();
  await expect(
    box.getByText("单节点扫描任务不支持", { exact: true }),
  ).toBeVisible();
  await box.getByRole("checkbox", { name: /移除 .*10\.0\.1\.13/ }).check();
  await expect(
    box.getByText("至少保留一个执行节点", { exact: true }),
  ).toBeVisible();
  await expect(
    box.getByRole("button", { name: "下一步", exact: true }),
  ).toBeDisabled();
});

test("local transfers require an absolute binding path and a passing preflight", async ({
  page,
}) => {
  let preflightValid = false;
  const { posts } = await setup(page, {
    view: () => nodesView({ localTransfer: true, direction: "OSS_TO_LOCAL" }),
    post: (body, route) => {
      const binding = {
        executionNodeIp: "10.0.1.14",
        localPath: body.bindings?.[0]?.localPath,
        valid: body.dryRun ? preflightValid : true,
        pathType: "DIRECTORY",
        code: preflightValid ? "" : "MARKER_MISMATCH",
        message: preflightValid ? "" : "共享目录标记不一致",
      };
      return route.fulfill({
        json: {
          success: true,
          data: {
            accepted: !body.dryRun,
            dryRun: !!body.dryRun,
            valid: body.dryRun ? preflightValid : true,
            nodeBindings: [binding],
            validationErrors: [],
            view: nodesView({ localTransfer: true }),
          },
        },
      });
    },
  });
  await openDialog(page);
  const box = dialog(page);
  await box.getByRole("checkbox", { name: /添加 .*10\.0\.1\.14/ }).check();
  const next = box.getByRole("button", { name: "下一步", exact: true });
  const preflight = box.getByRole("button", { name: "预检", exact: true });
  const path = box.getByRole("textbox", {
    name: /10\.0\.1\.14 的本地绝对路径/,
  });
  await expect(next).toBeDisabled();
  await expect(preflight).toBeDisabled();
  await path.fill("relative/dir");
  await expect(
    box.getByText("请输入执行节点上的绝对路径。", { exact: true }),
  ).toBeVisible();
  await expect(preflight).toBeDisabled();
  await path.fill("/mnt/share");
  await preflight.click();
  await expect(box.getByText("预检未通过", { exact: true })).toBeVisible();
  await expect(box.getByText("共享目录标记不一致").first()).toBeVisible();
  await expect(next).toBeDisabled();
  preflightValid = true;
  await preflight.click();
  await expect(box.getByText("预检通过").first()).toBeVisible();
  await expect(next).toBeEnabled();
  // Changing the path invalidates the earlier result.
  await path.fill("/mnt/share2");
  await expect(next).toBeDisabled();
  await preflight.click();
  await expect(next).toBeEnabled();
  await next.click();
  await expect(box.getByTestId("task-nodes-summary")).toContainText(
    "10.0.1.14 (/mnt/share2)",
  );
  await box.getByRole("button", { name: "确认变更", exact: true }).click();
  await expect(dialog(page)).toHaveCount(0);
  expect(posts.map((body) => !!body.dryRun)).toEqual([true, true, true, false]);
  expect(posts.at(-1)).toEqual({
    requestId: expect.stringMatching(/^[a-f0-9]{32}$/),
    add: ["10.0.1.14"],
    remove: [],
    bindings: [{ executionNodeIp: "10.0.1.14", localPath: "/mnt/share2" }],
    expectedGeneration: 1,
    expectedNodeRevision: 3,
  });
  expect(posts[0]).not.toHaveProperty("requestId");
});

test("a rejected change shows the server message and keeps the dialog open", async ({
  page,
}) => {
  const { posts, gets } = await setup(page, {
    post: (_, route) =>
      route.fulfill({
        status: 409,
        json: {
          success: false,
          code: "CHANGE_PENDING",
          message: "上一次节点变更尚未完成",
        },
      }),
  });
  await openDialog(page);
  const box = dialog(page);
  await box.getByRole("checkbox", { name: /添加 .*10\.0\.1\.14/ }).check();
  await box.getByRole("button", { name: "下一步", exact: true }).click();
  await box.getByRole("button", { name: "确认变更", exact: true }).click();
  await expect(
    box.getByText("上一次节点变更尚未完成", { exact: true }),
  ).toBeVisible();
  await expect(box.getByRole("button", { name: "下一步" })).toBeVisible();
  expect(posts).toHaveLength(1);
  // The rejected change reloads the current membership.
  await expect.poll(gets).toBeGreaterThanOrEqual(2);
  // A retry after a rejection uses a fresh request identity.
  await box.getByRole("button", { name: "下一步", exact: true }).click();
  await box.getByRole("button", { name: "确认变更", exact: true }).click();
  await expect.poll(() => posts.length).toBe(2);
  expect(posts[1].requestId).not.toBe(posts[0].requestId);
});

test("completed tasks have no edit action", async ({ page }) => {
  await setup(page, { taskId: 1045 });
  await page.goto("/tasks?taskId=1045");
  await expect(page.getByText("执行信息", { exact: true })).toBeVisible();
  await expect(page.getByTestId("task-membership-nodes")).toBeVisible();
  await expect(page.getByRole("button", { name: "编辑节点" })).toHaveCount(0);
});

test("non-editable tasks show the reason and disable every change", async ({
  page,
}) => {
  await setup(page, {
    view: () => nodesView({ editable: false, reason: "任务正在停止" }),
  });
  await openDialog(page);
  const box = dialog(page);
  await expect(box.getByText("任务正在停止", { exact: true })).toBeVisible();
  for (const checkbox of await box.getByRole("checkbox").all())
    await expect(checkbox).toBeDisabled();
  await expect(
    box.getByRole("button", { name: "下一步", exact: true }),
  ).toBeDisabled();
});

test("older masters without membership keep the plain node list", async ({
  page,
}) => {
  await setup(page, { taskExtra: { executionMembership: undefined } });
  await page.goto("/tasks?taskId=1048");
  await expect(page.getByText("执行信息", { exact: true })).toBeVisible();
  await expect(page.getByTestId("task-membership-nodes")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "编辑节点" })).toHaveCount(0);
});

test("pending changes show progress, refresh it and allow escalating a draining node", async ({
  page,
}) => {
  await installPausedClock(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  let remaining = 4;
  const view = () =>
    nodesView({
      pending: remaining > 0,
      nodes: [
        member("10.0.1.12", { scanner: true }),
        member("10.0.1.13", {
          state: remaining > 0 ? "DRAINING" : "ACTIVE",
          drainMode: "drain",
          pendingChildren: remaining,
        }),
        member("10.0.1.14", { state: "JOINING", share: null }),
      ],
    });
  const { posts } = await setup(page, { view });
  await page.goto("/tasks?taskId=1048");
  const edit = page.getByRole("button", { name: "编辑节点", exact: true });
  await advanceUntilVisible(page, edit);
  await edit.click();
  const box = dialog(page);
  await advanceUntilVisible(
    page,
    box.getByText("剩余 4 个子任务待移交或完成", { exact: true }),
  );
  await expect(box.getByText(/上一次节点变更仍在进行中/)).toBeVisible();
  await expect(
    box.getByText("正在完成握手与检查，加入后开始分配任务"),
  ).toBeVisible();
  remaining = 1;
  await advanceUntilVisible(
    page,
    box.getByText("剩余 1 个子任务待移交或完成", { exact: true }),
  );
  await box.getByRole("checkbox", { name: /移除 .*10\.0\.1\.13/ }).check();
  await box.getByRole("radio", { name: /^立即移除/ }).check();
  await box.getByRole("button", { name: "下一步", exact: true }).click();
  await box.getByRole("button", { name: "确认变更", exact: true }).click();
  await expect.poll(() => posts.length).toBe(1);
  expect(posts[0]).toMatchObject({ remove: ["10.0.1.13"], mode: "now" });
});
