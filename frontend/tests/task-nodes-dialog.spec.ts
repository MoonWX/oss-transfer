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

const footer = (page: Page) => dialog(page).getByTestId("task-nodes-footer");
const preview = (page: Page) =>
  dialog(page).getByRole("button", { name: "预览变更", exact: true });

async function pickAgent(page: Page, address: RegExp) {
  await dialog(page)
    .getByRole("button", { name: "添加 Agent", exact: true })
    .click();
  await page.getByRole("menuitem", { name: address }).click();
  await expect(page.getByRole("menu")).toHaveCount(0);
}

test("task detail lists node names with quiet states and adds an Agent after confirmation", async ({
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
  await expect(row.getByRole("listitem")).toHaveCount(4);
  await expect(row.getByText("移出中", { exact: true })).toHaveCount(2);
  await expect(row.getByText("加入中", { exact: true })).toBeVisible();
  // Shares and roles live in the dialog, not in the detail list.
  await expect(row).not.toContainText("带宽份额");
  await expect(row).not.toContainText("扫描");
  await expect(row.getByRole("listitem").first()).toHaveText(
    "master (10.0.1.12)",
  );
  await page.getByRole("button", { name: "编辑节点", exact: true }).click();
  const box = dialog(page);
  const scanner = box.getByTestId("task-node-10.0.1.12");
  await expect(scanner.getByText("扫描节点", { exact: true })).toBeVisible();
  await expect(scanner).toContainText(/带宽份额\s*不限/);
  await expect(box.getByTestId("task-node-10.0.1.13")).toContainText(
    /未完成子任务\s*2/,
  );
  await expect(preview(page)).toBeDisabled();
  await expect(footer(page)).toHaveText("选择要添加或移出的节点");
  await expect(box.getByText("1 个在线 Agent 可添加。")).toBeVisible();
  await box.getByRole("button", { name: "添加 Agent", exact: true }).click();
  const old = page.getByRole("menuitem", { name: /10\.0\.1\.15/ });
  await expect(old).toHaveAttribute("aria-disabled", "true");
  await expect(old).toContainText("Agent 版本过旧");
  await page.getByRole("menuitem", { name: /10\.0\.1\.14/ }).click();
  await expect(box.getByTestId("task-node-add-10.0.1.14")).toContainText(
    "将添加",
  );
  await expect(footer(page)).toHaveText("待提交：添加 1 个节点");
  await preview(page).click();
  const summary = box.getByTestId("task-nodes-summary");
  await expect(summary).toContainText("添加10.0.1.14");
  await expect(summary).not.toContainText("移出");
  await expect(footer(page)).toHaveText("变更将立即开始生效。");
  await box.getByRole("button", { name: "添加 1 个节点", exact: true }).click();
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
    await box.getByRole("button", { name: /^移出 .*10\.0\.1\.13/ }).click();
    const node = box.getByTestId("task-node-10.0.1.13");
    await expect(node).toHaveAttribute("data-marked", "remove");
    await expect(node).toContainText("将移出");
    await expect(box.getByRole("radio", { name: /^平滑移出/ })).toBeChecked();
    await expect(box.getByText("正在传输的子任务完成后离开")).toBeVisible();
    await expect(
      box.getByText("正在传输的子任务转给其他节点继续"),
    ).toBeVisible();
    await expect(footer(page)).toHaveText("待提交：移出 1 个节点");
    if (mode === "now")
      await box.getByRole("radio", { name: /^立即移出/ }).check();
    await preview(page).click();
    const summary = box.getByTestId("task-nodes-summary");
    await expect(summary).toContainText(
      mode === "now" ? "移出（立即移出）" : "移出（平滑移出）",
    );
    await expect(summary).toContainText("agent (10.0.1.13)");
    await box
      .getByRole("button", { name: "移出 1 个节点", exact: true })
      .click();
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

test("a marked removal can be undone from the keyboard and the mode choice disappears", async ({
  page,
}) => {
  await setup(page);
  await openDialog(page);
  const box = dialog(page);
  await box.getByRole("button", { name: /^移出 .*10\.0\.1\.13/ }).click();
  await expect(box.getByRole("radiogroup", { name: "移出方式" })).toBeVisible();
  const undo = box.getByRole("button", { name: /^撤销移出 .*10\.0\.1\.13/ });
  await undo.focus();
  await page.keyboard.press("Enter");
  await expect(box.getByRole("radio")).toHaveCount(0);
  await expect(box.getByTestId("task-node-10.0.1.13")).not.toHaveAttribute(
    "data-marked",
  );
  await expect(preview(page)).toBeDisabled();
  await expect(
    box.getByRole("button", { name: /^移出 .*10\.0\.1\.13/ }),
  ).toBeVisible();
});

test("remove now is disabled with the server reason and removing every node is blocked", async ({
  page,
}) => {
  await setup(page, {
    view: () =>
      nodesView({ nowSupported: false, nowReason: "单节点扫描任务不支持" }),
  });
  await openDialog(page);
  const box = dialog(page);
  await box.getByRole("button", { name: /^移出 .*10\.0\.1\.12/ }).click();
  await expect(box.getByRole("radio", { name: /^立即移出/ })).toBeDisabled();
  await expect(
    box.getByText("单节点扫描任务不支持", { exact: true }),
  ).toBeVisible();
  await box.getByRole("button", { name: /^移出 .*10\.0\.1\.13/ }).click();
  await expect(footer(page)).toHaveText("至少保留一个执行节点");
  await expect(preview(page)).toBeDisabled();
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
  await pickAgent(page, /10\.0\.1\.14/);
  const next = preview(page);
  const preflight = box.getByRole("button", { name: "预检", exact: true });
  const path = box.getByRole("textbox", {
    name: /10\.0\.1\.14 的本地绝对路径/,
  });
  await expect(next).toBeDisabled();
  await expect(footer(page)).toHaveText("填写新节点的本地绝对路径后才能继续");
  await expect(preflight).toBeDisabled();
  await path.fill("relative/dir");
  await expect(
    box.getByText("请输入执行节点上的绝对路径。", { exact: true }),
  ).toBeVisible();
  await expect(preflight).toBeDisabled();
  await path.fill("/mnt/share");
  await expect(footer(page)).toHaveText("预检通过后才能继续");
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
  await box.getByRole("button", { name: "添加 1 个节点", exact: true }).click();
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

test("a chosen Agent can be dropped again before submitting", async ({
  page,
}) => {
  await setup(page);
  await openDialog(page);
  const box = dialog(page);
  await pickAgent(page, /10\.0\.1\.14/);
  await box.getByRole("button", { name: /^撤销添加 .*10\.0\.1\.14/ }).click();
  await expect(box.getByTestId("task-node-add-10.0.1.14")).toHaveCount(0);
  await expect(preview(page)).toBeDisabled();
});

test("without online Agents the add section shows an empty state", async ({
  page,
}) => {
  await setup(page, { view: () => nodesView({ candidates: [] }) });
  await openDialog(page);
  const box = dialog(page);
  await expect(box.getByText("没有可添加的在线 Agent")).toBeVisible();
  await expect(box.getByRole("button", { name: "添加 Agent" })).toHaveCount(0);
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
  await pickAgent(page, /10\.0\.1\.14/);
  await preview(page).click();
  await box.getByRole("button", { name: "添加 1 个节点", exact: true }).click();
  await expect(
    box.getByText("上一次节点变更尚未完成", { exact: true }),
  ).toBeVisible();
  await expect(preview(page)).toBeVisible();
  expect(posts).toHaveLength(1);
  // The rejected change reloads the current membership.
  await expect.poll(gets).toBeGreaterThanOrEqual(2);
  // A retry after a rejection uses a fresh request identity.
  await preview(page).click();
  await box.getByRole("button", { name: "添加 1 个节点", exact: true }).click();
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
  await expect(box.getByRole("alert").getByText("任务正在停止")).toBeVisible();
  const removals = box.getByRole("button", { name: /^移出 / });
  await expect(removals).toHaveCount(2);
  for (const button of await removals.all())
    await expect(button).toBeDisabled();
  await expect(
    box.getByRole("button", { name: "添加 Agent", exact: true }),
  ).toBeDisabled();
  await expect(preview(page)).toBeDisabled();
  await expect(footer(page)).toHaveText("任务正在停止");
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
  const draining = box.getByTestId("task-node-10.0.1.13");
  await advanceUntilVisible(
    page,
    draining.filter({ hasText: /待完成或移交\s*4(?!\d)/ }),
  );
  await expect(draining.getByText("平滑移出中", { exact: true })).toBeVisible();
  await expect(box.getByText(/上一次节点变更仍在进行中/)).toBeVisible();
  await expect(
    box.getByText("正在完成握手与检查，加入后开始分配任务"),
  ).toBeVisible();
  await expect(
    box.getByRole("button", { name: /^取消加入 .*10\.0\.1\.14/ }),
  ).toBeVisible();
  remaining = 1;
  await advanceUntilVisible(
    page,
    draining.filter({ hasText: /待完成或移交\s*1(?!\d)/ }),
  );
  await box
    .getByRole("button", { name: /^改为立即移出 .*10\.0\.1\.13/ })
    .click();
  await expect(box.getByRole("radio", { name: /^立即移出/ })).toBeChecked();
  await preview(page).click();
  await box.getByRole("button", { name: "移出 1 个节点", exact: true }).click();
  await expect.poll(() => posts.length).toBe(1);
  expect(posts[0]).toMatchObject({ remove: ["10.0.1.13"], mode: "now" });
});

test("cancelling a joining node submits its removal", async ({ page }) => {
  const { posts } = await setup(page, {
    view: () =>
      nodesView({
        pending: true,
        nodes: [
          member("10.0.1.12", { scanner: true }),
          member("10.0.1.14", { state: "JOINING", share: null }),
        ],
      }),
  });
  await openDialog(page);
  const box = dialog(page);
  await box.getByRole("button", { name: /^取消加入 .*10\.0\.1\.14/ }).click();
  await preview(page).click();
  await box.getByRole("button", { name: "移出 1 个节点", exact: true }).click();
  await expect.poll(() => posts.length).toBe(1);
  expect(posts[0]).toMatchObject({ add: [], remove: ["10.0.1.14"] });
});

test("the dialog fills a phone screen and keeps the footer reachable", async ({
  page,
}) => {
  await page.setViewportSize({ width: 375, height: 700 });
  await setup(page);
  await openDialog(page);
  const box = dialog(page);
  expect((await box.boundingBox())?.width).toBe(375);
  await box.getByRole("button", { name: /^移出 .*10\.0\.1\.13/ }).click();
  await expect(footer(page)).toBeInViewport();
  await expect(preview(page)).toBeInViewport();
});

test("a draining node can only be kept when the task cannot remove nodes now", async ({
  page,
}) => {
  await setup(page, {
    view: () =>
      nodesView({
        pending: true,
        nowSupported: false,
        nowReason: "独占目录不支持接管",
        nodes: [
          member("10.0.1.12", { scanner: true }),
          member("10.0.1.13", { state: "DRAINING", drainMode: "drain" }),
        ],
      }),
  });
  await openDialog(page);
  const node = dialog(page).getByTestId("task-node-10.0.1.13");
  await expect(node.getByRole("button")).toHaveCount(1);
  await expect(
    node.getByRole("button", { name: /^保留 .*10\.0\.1\.13/ }),
  ).toBeEnabled();
});

async function openLiveDialog(page: Page) {
  await page.goto("/tasks?taskId=1048");
  const edit = page.getByRole("button", { name: "编辑节点", exact: true });
  await advanceUntilVisible(page, edit);
  await edit.click();
  return dialog(page);
}

async function refreshEverySecond(page: Page) {
  await installPausedClock(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
}

test("the open dialog refreshes statistics live and keeps selections, paths and preflight results", async ({
  page,
}) => {
  await refreshEverySecond(page);
  let round = 0;
  const view = () =>
    nodesView({
      localTransfer: true,
      direction: "OSS_TO_LOCAL",
      nodes: [
        member("10.0.1.12", { scanner: true, pendingChildren: 5 + round * 4 }),
        member("10.0.1.13", {
          pendingChildren: 2,
          share: { bandwidthBytesPerSecond: round > 0 ? 1048576 : 0 },
        }),
      ],
    });
  const { gets } = await setup(page, {
    view,
    post: (body, route) =>
      route.fulfill({
        json: {
          success: true,
          data: {
            accepted: !body.dryRun,
            dryRun: !!body.dryRun,
            valid: true,
            nodeBindings: [
              {
                executionNodeIp: "10.0.1.14",
                localPath: body.bindings?.[0]?.localPath,
                valid: true,
                pathType: "DIRECTORY",
              },
            ],
            validationErrors: [],
            view: view(),
          },
        },
      }),
  });
  const box = await openLiveDialog(page);
  const scanner = box.getByTestId("task-node-10.0.1.12");
  const leaving = box.getByTestId("task-node-10.0.1.13");
  await advanceUntilVisible(
    page,
    scanner.filter({ hasText: /未完成子任务\s*5(?!\d)/ }),
  );
  // Choose a removal and an addition with its path, and pass the preflight.
  await box.getByRole("button", { name: /^移出 .*10\.0\.1\.13/ }).click();
  await box.getByRole("button", { name: "添加 Agent", exact: true }).click();
  await page.getByRole("menuitem", { name: /10\.0\.1\.14/ }).click();
  const path = box.getByRole("textbox", {
    name: /10\.0\.1\.14 的本地绝对路径/,
  });
  await advanceUntilVisible(page, path); // the menu closes on the paused clock
  await path.fill("/mnt/share");
  await box.getByRole("button", { name: "预检", exact: true }).click();
  const passed = box.getByRole("alert").filter({ hasText: "预检通过" });
  await expect(passed).toBeVisible();
  // Statistics follow the Master while every choice stays as it was.
  round = 1;
  await advanceUntilVisible(
    page,
    scanner.filter({ hasText: /未完成子任务\s*9(?!\d)/ }),
  );
  await expect(leaving).toContainText(/带宽份额\s*1(\.0)? MiB\/s/);
  await expect(leaving).toHaveAttribute("data-marked", "remove");
  await expect(box.getByTestId("task-node-add-10.0.1.14")).toContainText(
    "将添加",
  );
  await expect(path).toHaveValue("/mnt/share");
  await expect(passed).toBeVisible();
  await expect(footer(page)).toHaveText(
    "待提交：添加 1 个节点 · 移出 1 个节点",
  );
  await expect(preview(page)).toBeEnabled();
  // The confirmation step keeps the view it summarises.
  await preview(page).click();
  const summary = box.getByTestId("task-nodes-summary");
  await expect(summary).toContainText("10.0.1.14 (/mnt/share)");
  const confirming = gets();
  round = 2;
  await page.clock.runFor(5000);
  expect(gets()).toBe(confirming);
  await expect(summary).toContainText("10.0.1.14 (/mnt/share)");
  await box.getByRole("button", { name: "返回修改", exact: true }).click();
  await advanceUntilVisible(
    page,
    scanner.filter({ hasText: /未完成子任务\s*13(?!\d)/ }),
  );
  await expect(leaving).toHaveAttribute("data-marked", "remove");
  await expect(path).toHaveValue("/mnt/share");
  await expect(passed).toBeVisible();
});

test("a hidden page pauses the dialog refresh and one failed refresh keeps the last view", async ({
  page,
}) => {
  await refreshEverySecond(page);
  let pendingChildren = 2;
  let failures = 0;
  const { gets } = await setup(page, {
    view: () =>
      nodesView({
        nodes: [
          member("10.0.1.12", { scanner: true }),
          member("10.0.1.13", { pendingChildren }),
        ],
      }),
  });
  await page.route("**/api/master/tasks/1048/nodes", async (route) => {
    if (route.request().method() === "GET" && failures > 0) {
      failures--;
      await route.fulfill({
        status: 500,
        json: { success: false, message: "暂时不可用" },
      });
      return;
    }
    await route.fallback();
  });
  const box = await openLiveDialog(page);
  const node = box.getByTestId("task-node-10.0.1.13");
  await advanceUntilVisible(
    page,
    node.filter({ hasText: /未完成子任务\s*2(?!\d)/ }),
  );
  const setHidden = (hidden: boolean) =>
    page.evaluate((value) => {
      Object.defineProperty(document, "hidden", {
        configurable: true,
        get: () => value,
      });
      document.dispatchEvent(new Event("visibilitychange"));
    }, hidden);
  await setHidden(true);
  const hidden = gets();
  pendingChildren = 6;
  await page.clock.runFor(5000);
  expect(gets()).toBe(hidden);
  // Becoming visible refreshes at once, without waiting for the interval.
  await setHidden(false);
  await expect(node).toContainText(/未完成子任务\s*6(?!\d)/);
  // One failed background refresh keeps the view quietly; two are reported.
  failures = 1;
  pendingChildren = 7;
  await advanceUntilVisible(
    page,
    node.filter({ hasText: /未完成子任务\s*7(?!\d)/ }),
  );
  await expect(box.getByText(/无法读取执行节点/)).toHaveCount(0);
  failures = 2;
  await advanceUntilVisible(page, box.getByText(/无法读取执行节点/));
  await expect(node).toContainText(/未完成子任务\s*7(?!\d)/);
});

test("the scan state replaces the scanner tag once the scan has finished", async ({
  page,
}) => {
  await refreshEverySecond(page);
  let phase = "HANDOFF";
  await setup(page, {
    view: () =>
      nodesView({
        pending: phase === "HANDOFF",
        scan: { phase, complete: phase === "COMPLETE" },
        nodes: [
          member("10.0.1.12", { scanner: phase === "HANDOFF" }),
          member("10.0.1.13", {
            state: phase === "HANDOFF" ? "DRAINING" : "ACTIVE",
            drainMode: phase === "HANDOFF" ? "drain" : "",
            online: phase !== "HANDOFF",
            ...(phase === "HANDOFF" ? { progress: "LEASE_EXPIRY" } : {}),
          }),
        ],
      }),
  });
  const box = await openLiveDialog(page);
  const scan = box.getByTestId("task-nodes-scan");
  // A new scanner taking over is labelled, and the handoff is explained.
  await advanceUntilVisible(page, scan.filter({ hasText: "正在移交扫描" }));
  await expect(
    box
      .getByTestId("task-node-10.0.1.12")
      .getByText("扫描节点", { exact: true }),
  ).toBeVisible();
  await expect(box.getByTestId("task-node-10.0.1.13")).toContainText(
    "节点离线，执行租约到期后离开，其任务由其他节点继续",
  );
  phase = "COMPLETE";
  await advanceUntilVisible(page, box.getByText("扫描已完成", { exact: true }));
  await expect(scan).toHaveText("扫描已完成，不再需要扫描节点");
  await expect(box.getByText("扫描节点", { exact: true })).toHaveCount(0);
  await expect(box.getByTestId("task-node-10.0.1.13")).not.toContainText(
    "执行租约到期",
  );
});

test("without lease fencing an offline leaving node is shown waiting to reconnect", async ({
  page,
}) => {
  await setup(page, {
    view: () =>
      nodesView({
        pending: true,
        nodes: [
          member("10.0.1.12", { scanner: true }),
          member("10.0.1.13", {
            state: "DRAINING",
            drainMode: "drain",
            online: false,
            progress: "RECONNECT",
          }),
        ],
      }),
  });
  await openDialog(page);
  const node = dialog(page).getByTestId("task-node-10.0.1.13");
  await expect(node).toContainText(
    "节点离线，重新连接并完成或移交其任务后离开",
  );
  await expect(node).not.toContainText("执行租约到期");
});

test("a refresh still in flight when the change is previewed does not alter the confirmation", async ({
  page,
}) => {
  await refreshEverySecond(page);
  let eligible = true;
  await setup(page, {
    view: () =>
      nodesView({
        candidates: [
          { id: "node-14", address: "10.0.1.14", eligible, reason: "" },
        ],
      }),
  });
  let hold: Promise<void> | undefined;
  let release: () => void = () => {};
  let held = 0;
  await page.route("**/api/master/tasks/1048/nodes", async (route) => {
    if (route.request().method() === "GET" && hold) {
      held++;
      await hold;
      // The page may have cancelled this read.
      await route.fallback().catch(() => undefined);
      return;
    }
    await route.fallback();
  });
  const box = await openLiveDialog(page);
  await box.getByRole("button", { name: "添加 Agent", exact: true }).click();
  await page.getByRole("menuitem", { name: /10\.0\.1\.14/ }).click();
  const added = box.getByTestId("task-node-add-10.0.1.14");
  await advanceUntilVisible(page, added);
  // A background read starts, and the Master's answer will no longer offer
  // the chosen Agent.
  hold = new Promise<void>((resolve) => (release = resolve));
  await expect
    .poll(async () => {
      await page.clock.runFor(250);
      return held;
    })
    .toBeGreaterThan(0);
  eligible = false;
  await preview(page).click();
  const summary = box.getByTestId("task-nodes-summary");
  await expect(summary).toContainText("10.0.1.14");
  release();
  hold = undefined;
  // Give a late response every chance to land before checking again.
  await page.waitForTimeout(500);
  await expect(summary).toContainText("10.0.1.14");
  await expect(
    box.getByRole("button", { name: "添加 1 个节点", exact: true }),
  ).toBeEnabled();
});
