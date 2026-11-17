import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

const nodes = ["10.0.1.12", "10.0.1.13"];

async function select(page: Page, label: string, option: string) {
  await page.getByRole("combobox", { name: label }).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}

// An upload from one shared directory, first on one node.
async function configureUpload(page: Page) {
  const requests = await mockApi(page);
  const validations: any[] = [];
  // The shared fixture answers for one node; this page binds two.
  await page.route("**/api/master/local-transfers/validate", (route) => {
    const row = route.request().postDataJSON()[0];
    validations.push(row);
    // As the Master answers: a skipped check passes the paths only.
    const skipped = row.skipSharedCheck === true;
    return route.fulfill({
      json: {
        success: true,
        data: {
          valid: true,
          nodeBindings: nodes.map((executionNodeIp) => ({
            executionNodeIp,
            localPath: "/mnt/datasets/qa",
            valid: true,
            pathType: "DIRECTORY",
            ...(skipped
              ? {
                  code: "SHARED_STORAGE_CHECK_SKIPPED",
                  message:
                    "已按要求跳过共享目录校验；未验证各节点是否挂载同一份存储",
                }
              : { code: "SHARED_STORAGE_VERIFIED" }),
          })),
        },
      },
    });
  });
  await page.goto("/create");
  await page.getByRole("button", { name: /本地 → 对象存储/ }).click();
  await select(page, "目标存储连接", "backup");
  await page.getByRole("combobox", { name: "目标存储桶" }).fill("media-backup");
  await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
  await page.getByRole("option", { name: /10\.0\.1\.12/ }).click();
  await page.getByLabel("源文件或目录绝对路径").fill("/mnt/datasets/qa");
  return { requests, validations };
}

const skipBox = (page: Page) =>
  page.getByRole("checkbox", { name: "跳过共享目录校验（不推荐）" });
const dialog = (page: Page) =>
  page.getByRole("dialog", { name: "跳过共享目录校验？" });

test("skipping the shared check needs an explicit confirmation and sends the acknowledgement", async ({
  page,
}) => {
  const { requests, validations } = await configureUpload(page);
  // One node has nothing to compare with, so the option is not offered.
  await expect(skipBox(page)).toHaveCount(0);
  await page.getByRole("button", { name: "添加共享存储节点" }).click();
  await expect(skipBox(page)).not.toBeChecked();

  await skipBox(page).click();
  await expect(dialog(page)).toBeVisible();
  for (const consequence of [
    "不同节点会从不同的数据中读取对象；",
    "部分文件可能漏传，或从错误的副本上传；",
    "任务报告可能显示成功，而目标端实际不完整或不一致；",
    "节点失联后的自动接管也不做校验，可能在另一个目录上继续执行。",
  ])
    await expect(dialog(page).getByText(consequence)).toBeVisible();
  // A node added later is checked unless that change skips the check itself.
  await expect(
    dialog(page).getByText(
      "之后添加节点时仍会校验共享目录，除非届时再次确认跳过。",
    ),
  ).toBeVisible();
  const confirm = dialog(page).getByRole("button", { name: "跳过校验" });
  await expect(confirm).toBeDisabled();
  await dialog(page).getByRole("button", { name: "取消" }).click();
  await expect(dialog(page)).toHaveCount(0);
  await expect(skipBox(page)).not.toBeChecked();
  await expect(page.getByTestId("skip-shared-check-warning")).toHaveCount(0);

  await skipBox(page).click();
  // The acknowledgement from the previous opening is not remembered.
  await expect(confirm).toBeDisabled();
  await dialog(page)
    .getByRole("checkbox", { name: "我已确认所有执行节点挂载的是同一份存储" })
    .check();
  await confirm.click();
  await expect(dialog(page)).toHaveCount(0);
  await expect(skipBox(page)).toBeChecked();
  await expect(page.getByTestId("skip-shared-check-warning")).toBeVisible();
  await expect(page.getByText(/ · 已跳过共享目录校验$/)).toBeVisible();

  await page.getByRole("button", { name: "运行预检", exact: true }).click();
  // The paths passed; the shared directory was not checked, and says so.
  await expect(
    page.getByText("路径预检通过，共享目录未校验", { exact: true }).last(),
  ).toBeVisible();
  await expect(page.getByText("节点预检通过", { exact: true })).toHaveCount(0);
  await expect(page.getByText("未校验", { exact: true })).toHaveCount(2);
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
  const creation = () =>
    requests.find(
      (r) => r.path === "/api/master/local-transfers" && r.method === "POST",
    );
  await expect.poll(creation).toBeTruthy();
  expect(validations).toHaveLength(1);
  for (const body of [validations[0], creation()!.body[0]]) {
    expect(body.skipSharedCheck).toBe(true);
    expect(body.skipSharedCheckAcknowledged).toBe(true);
    expect(body.sourceEndpoint.nodeBindings).toHaveLength(2);
  }
});

test("the skip is dropped with the second node and never sent by default", async ({
  page,
}) => {
  const { validations } = await configureUpload(page);
  await page.getByRole("button", { name: "添加共享存储节点" }).click();
  await skipBox(page).click();
  await dialog(page)
    .getByRole("checkbox", { name: "我已确认所有执行节点挂载的是同一份存储" })
    .check();
  await dialog(page).getByRole("button", { name: "跳过校验" }).click();
  await expect(skipBox(page)).toBeChecked();

  await page.getByRole("button", { name: "移除执行节点 2" }).click();
  await expect(skipBox(page)).toHaveCount(0);
  await page.getByRole("button", { name: "添加共享存储节点" }).click();
  await expect(skipBox(page)).not.toBeChecked();

  await page.getByRole("button", { name: "运行预检", exact: true }).click();
  await expect(page.getByText("所有节点预检通过").first()).toBeVisible();
  expect(validations).toHaveLength(1);
  const [validation] = validations;
  expect(validation).not.toHaveProperty("skipSharedCheck");
  expect(validation).not.toHaveProperty("skipSharedCheckAcknowledged");
});

test("task details keep warning about a skipped shared check", async ({
  page,
}) => {
  await mockApi(page);
  const rows = tasks.map((task) =>
    task.taskIndex === 1046 ? { ...task, sharedCheckSkipped: true } : task,
  );
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: { success: true, totalTasks: rows.length, tasks: rows },
    }),
  );
  await page.goto("/tasks?taskId=1046");
  const warning = page.getByTestId("task-shared-check-skipped");
  await expect(warning).toContainText("此任务跳过了共享目录校验");
  await expect(warning).toContainText("即使任务显示成功");
  // Task 1046 is a download: its consequence is a split destination.
  await expect(warning).toContainText("下载的文件会分散写入各节点各自的目录");
  // A node change can record the skip too, and later nodes are still checked.
  await expect(warning).toContainText("创建任务或添加节点时");
  await expect(warning).not.toContainText("创建时未验证");
  await expect(warning).toContainText(
    "之后添加节点时仍会校验共享目录，除非届时再次确认跳过",
  );
  // Takeover goes unchecked only where those nodes take the work over.
  await expect(warning).toContainText(
    "由这些节点接管失联节点的工作时也不做校验",
  );
  await expect(warning).not.toContainText("从错误的副本上传");
  await page.goto("/tasks?taskId=1048");
  await expect(page.getByText("执行信息", { exact: true })).toBeVisible();
  await expect(page.getByTestId("task-shared-check-skipped")).toHaveCount(0);
});

test("a download states a download's consequences and the check it keeps", async ({
  page,
}) => {
  await mockApi(page);
  await page.goto("/create");
  await page.getByRole("button", { name: /对象存储 → 本地/ }).click();
  await select(page, "源存储连接", "production");
  await page.getByRole("combobox", { name: "源存储桶" }).fill("media-archive");
  await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
  await page.getByRole("option", { name: /10\.0\.1\.12/ }).click();
  await page.getByLabel("目标目录绝对路径").fill("/mnt/datasets/media");
  await page.getByRole("button", { name: "添加共享存储节点" }).click();
  await expect(
    page.getByText("下载目标必须可写", { exact: false }),
  ).toBeVisible();
  await skipBox(page).click();
  for (const consequence of [
    "下载的文件会分散写入各节点各自的目录，没有一个目录包含完整的数据；",
    "冲突检查（跳过或覆盖已存在的文件）只会检查各节点自己的目录；",
    "任务报告可能显示成功，而本地目标实际不完整；",
    "节点失联后的自动接管也不做校验，可能写入另一个目录。",
  ])
    await expect(dialog(page).getByText(consequence)).toBeVisible();
  await expect(dialog(page).getByText(/从错误的副本上传/)).toHaveCount(0);
  await expect(dialog(page).getByText(/抽样比对/)).toHaveCount(0);
  await dialog(page)
    .getByRole("checkbox", { name: "我已确认所有执行节点挂载的是同一份存储" })
    .check();
  await dialog(page).getByRole("button", { name: "跳过校验" }).click();
  await expect(page.getByTestId("skip-shared-check-warning")).toContainText(
    "下载的文件会分散在各节点各自的目录中",
  );
});

test("imported rows that skip the check are named with the consequences", async ({
  page,
}) => {
  await mockApi(page);
  await page.goto("/create");
  await page.getByRole("tab", { name: "批量导入" }).click();
  await select(page, "导入类型", "本地迁移请求");
  const row = (skip: boolean) => ({
    direction: "LOCAL_TO_OSS",
    sourceEndpoint: {
      type: "LOCAL",
      nodeBindings: nodes.map((executionNodeIp) => ({
        executionNodeIp,
        localPath: "/mnt/datasets/qa",
      })),
    },
    destinationEndpoint: { type: "OSS", s3Alias: "backup", bucket: "b" },
    ...(skip
      ? { skipSharedCheck: true, skipSharedCheckAcknowledged: true }
      : {}),
  });
  const warning = page.getByTestId("batch-skip-shared-check-warning");
  await page
    .getByLabel("任务数据")
    .fill(JSON.stringify([row(false), row(false)]));
  await expect(warning).toHaveCount(0);
  await page
    .getByLabel("任务数据")
    .fill(JSON.stringify([row(false), row(true), row(true)]));
  await expect(warning).toContainText("第 2, 3 项跳过了共享目录校验");
  await expect(warning).toContainText("下载的文件会分散在各节点各自的目录中");
  await expect(warning).toContainText("之后添加节点时仍会校验共享目录");
});

test("a restored draft says that a skipped submission may already be accepted", async ({
  page,
}) => {
  await configureUpload(page);
  await page.getByRole("button", { name: "添加共享存储节点" }).click();
  await skipBox(page).click();
  await dialog(page)
    .getByRole("checkbox", { name: "我已确认所有执行节点挂载的是同一份存储" })
    .check();
  await dialog(page).getByRole("button", { name: "跳过校验" }).click();
  // A submission was sent but its response never arrived.
  await page.evaluate(() => {
    const draft = JSON.parse(
      sessionStorage.getItem("oss-transfer-create-draft")!,
    );
    draft.pendingRequest = { id: "a".repeat(32), fingerprint: "sent" };
    sessionStorage.setItem("oss-transfer-create-draft", JSON.stringify(draft));
  });
  await page.reload();
  await expect(page.getByTestId("draft-skipped-submission")).toContainText(
    "该提交可能已被受理",
  );
  // The skip itself is never restored without a fresh confirmation.
  await expect(skipBox(page)).not.toBeChecked();
});

const member = (address: string) => ({
  id: `node-${address}`,
  address,
  state: "ACTIVE",
  drainMode: "",
  scanner: address === "10.0.1.12",
  online: true,
  pendingChildren: 0,
  localPath: "/mnt/share",
  share: { bandwidthBytesPerSecond: 0, listQps: 0, headQps: 0, putQps: 0 },
});

/**
 * Opens the node dialog of task 1048, a local transfer on two nodes, and
 * answers each node change as the Master does: the added node is checked
 * unless the request skips the check with its acknowledgement.
 */
async function openNodeChange(
  page: Page,
  options: { direction?: string; sharedCheckSkipped?: boolean } = {},
) {
  await mockApi(page);
  const view = {
    taskId: 1048,
    generation: 1,
    nodeRevision: 3,
    status: "RUNNING",
    direction: options.direction ?? "LOCAL_TO_OSS",
    localTransfer: true,
    ...(options.sharedCheckSkipped ? { sharedCheckSkipped: true } : {}),
    editable: true,
    reason: "",
    nowSupported: true,
    nowReason: "",
    pending: false,
    bandwidthLimitMbps: null,
    nodes: [member("10.0.1.12"), member("10.0.1.13")],
    removedNodes: [],
    candidates: [
      { id: "node-14", address: "10.0.1.14", eligible: true, reason: "" },
    ],
  };
  const posts: any[] = [];
  await page.route("**/api/master/tasks/1048/nodes", async (route) => {
    if (route.request().method() === "GET")
      return route.fulfill({ json: { success: true, data: view } });
    const body = route.request().postDataJSON();
    posts.push(body);
    const skipped = body.skipSharedCheck === true;
    return route.fulfill({
      json: {
        success: true,
        data: {
          accepted: !body.dryRun,
          dryRun: !!body.dryRun,
          replayed: false,
          valid: true,
          nodeBindings: [
            {
              executionNodeIp: "10.0.1.14",
              localPath: "/mnt/share",
              valid: true,
              pathType: "DIRECTORY",
              ...(skipped
                ? {
                    code: "SHARED_STORAGE_CHECK_SKIPPED",
                    message:
                      "已按要求跳过共享目录校验；未验证各节点是否挂载同一份存储",
                  }
                : {
                    code: "SHARED_STORAGE_VERIFIED",
                    message: "已写入测试文件，所有节点读取校验通过并已清理",
                  }),
            },
          ],
          validationErrors: [],
          view,
        },
      },
    });
  });
  // The task list offers node changes for tasks that report their members.
  const rows = tasks.map((task) =>
    task.taskIndex === 1048
      ? {
          ...task,
          executionMembership: {
            nodeRevision: 3,
            pending: false,
            nodes: view.nodes,
            joining: [],
            draining: [],
          },
        }
      : task,
  );
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: { success: true, totalTasks: rows.length, tasks: rows },
    }),
  );
  await page.goto("/tasks?taskId=1048");
  await page.getByRole("button", { name: "编辑节点", exact: true }).click();
  const box = page.getByRole("dialog", { name: "任务 #1048 执行节点" });
  await box.getByRole("button", { name: "添加 Agent", exact: true }).click();
  await page.getByRole("menuitem", { name: /10\.0\.1\.14/ }).click();
  await box
    .getByRole("textbox", { name: /10\.0\.1\.14 的本地绝对路径/ })
    .fill("/mnt/share");
  return { box, posts };
}

const joinSkipBox = (page: Page) =>
  page.getByRole("checkbox", { name: "跳过新节点的共享目录校验（不推荐）" });
const joinDialog = (page: Page) =>
  page.getByRole("dialog", { name: "跳过新节点的共享目录校验？" });

test("nodes added to a skipped task are checked unless the change skips the check itself", async ({
  page,
}) => {
  const { box, posts } = await openNodeChange(page, {
    sharedCheckSkipped: true,
  });
  // The task's earlier skip is stated, and so is what happens to the new node.
  const notice = box.getByTestId("task-nodes-skipped-task");
  await expect(notice).toContainText("此任务跳过了共享目录校验");
  await expect(notice).toContainText("本次添加的节点仍会校验共享目录");
  await expect(joinSkipBox(page)).not.toBeChecked();
  await box.getByRole("button", { name: "预检", exact: true }).click();
  const result = box.getByTestId("task-nodes-preflight");
  await expect(result).toContainText("预检通过");
  // The Master's note is shown for a node that passed, too.
  await expect(result).toContainText(
    "已写入测试文件，所有节点读取校验通过并已清理",
  );
  await expect(result).toContainText("通过 · 目录");
  await box.getByRole("button", { name: "预览变更", exact: true }).click();
  await expect(box.getByTestId("task-nodes-summary")).toContainText(
    "此任务跳过了共享目录校验；本次添加的节点已通过校验",
  );
  await box.getByRole("button", { name: "添加 1 个节点", exact: true }).click();
  await expect.poll(() => posts.filter((body) => !body.dryRun).length).toBe(1);
  // Neither the dry run nor the change asked to skip anything.
  for (const body of posts) {
    expect(body).not.toHaveProperty("skipSharedCheck");
    expect(body).not.toHaveProperty("skipSharedCheckAcknowledged");
  }
});

test("skipping the check for added nodes needs the consequences and sends both fields", async ({
  page,
}) => {
  const { box, posts } = await openNodeChange(page);
  // Not a skipped task: nothing to say about an earlier skip.
  await expect(box.getByTestId("task-nodes-skipped-task")).toHaveCount(0);
  await box.getByRole("button", { name: "预检", exact: true }).click();
  await expect(box.getByTestId("task-nodes-preflight")).toContainText(
    "预检通过",
  );

  await joinSkipBox(page).click();
  await expect(joinDialog(page)).toBeVisible();
  for (const consequence of [
    "新节点会从与其他节点不同的数据中读取对象；",
    "部分文件可能漏传，或从错误的副本上传；",
    "任务报告可能显示成功，而目标端实际不完整或不一致；",
    "新节点接管失联节点的工作时也不做校验，可能在另一个目录上继续执行。",
  ])
    await expect(joinDialog(page).getByText(consequence)).toBeVisible();
  // Only the added nodes skip the check: the others stay verified.
  await expect(
    joinDialog(page).getByText(
      "任务的其他节点不受影响，运行期间仍会校验共享目录。",
    ),
  ).toBeVisible();
  await expect(joinDialog(page).getByText(/之后添加节点时/)).toHaveCount(0);
  const confirm = joinDialog(page).getByRole("button", { name: "跳过校验" });
  await expect(confirm).toBeDisabled();
  await joinDialog(page).getByRole("button", { name: "取消" }).click();
  await expect(joinDialog(page)).toHaveCount(0);
  await expect(joinSkipBox(page)).not.toBeChecked();

  await joinSkipBox(page).click();
  // The acknowledgement from the previous opening is not remembered.
  await expect(confirm).toBeDisabled();
  await joinDialog(page)
    .getByRole("checkbox", { name: "我已确认新节点挂载的是同一份存储" })
    .check();
  await confirm.click();
  await expect(joinDialog(page)).toHaveCount(0);
  await expect(joinSkipBox(page)).toBeChecked();
  await expect(box.getByTestId("task-nodes-skip-shared-check")).toContainText(
    "已跳过新节点的共享目录校验",
  );
  // The earlier result checked the shared directory; it no longer applies.
  await expect(box.getByTestId("task-nodes-preflight")).toHaveCount(0);
  const next = box.getByRole("button", { name: "预览变更", exact: true });
  await expect(next).toBeDisabled();
  await box.getByRole("button", { name: "预检", exact: true }).click();
  const result = box.getByTestId("task-nodes-preflight");
  await expect(result).toContainText("路径检查通过，共享目录未校验");
  await expect(result).toContainText("已按要求跳过共享目录校验");
  await expect(result.getByText("未校验", { exact: true })).toBeVisible();
  await expect(
    box.getByText("未校验共享目录（已跳过校验）", { exact: true }),
  ).toBeVisible();
  await next.click();
  await expect(box.getByTestId("task-nodes-summary")).toContainText(
    "已跳过共享目录校验，未验证新节点挂载的是同一份存储",
  );
  await box.getByRole("button", { name: "添加 1 个节点", exact: true }).click();
  await expect.poll(() => posts.filter((body) => !body.dryRun).length).toBe(1);
  const [plain, skippedRun, change] = posts;
  expect(plain).not.toHaveProperty("skipSharedCheck");
  for (const body of [skippedRun, change])
    expect(body).toMatchObject({
      add: ["10.0.1.14"],
      skipSharedCheck: true,
      skipSharedCheckAcknowledged: true,
    });
  expect(skippedRun.dryRun).toBe(true);
  expect(change.dryRun).toBeUndefined();
});

test("a download's node change states a download's consequences and drops the skip with the node", async ({
  page,
}) => {
  const { box, posts } = await openNodeChange(page, {
    direction: "OSS_TO_LOCAL",
    sharedCheckSkipped: true,
  });
  await expect(
    box.getByText(/预检仍会检查新节点能否写入下载目标/),
  ).toBeVisible();
  await joinSkipBox(page).click();
  for (const consequence of [
    "新节点下载的文件会写入其他节点看不到的目录，没有一个目录包含完整的数据；",
    "冲突检查（跳过或覆盖已存在的文件）只会检查各节点自己的目录；",
    "任务报告可能显示成功，而本地目标实际不完整；",
    "新节点接管失联节点的工作时也不做校验，可能写入另一个目录。",
  ])
    await expect(joinDialog(page).getByText(consequence)).toBeVisible();
  await expect(joinDialog(page).getByText(/从错误的副本上传/)).toHaveCount(0);
  await expect(joinDialog(page).getByText(/抽样比对/)).toHaveCount(0);
  await joinDialog(page)
    .getByRole("checkbox", { name: "我已确认新节点挂载的是同一份存储" })
    .check();
  await joinDialog(page).getByRole("button", { name: "跳过校验" }).click();
  await expect(box.getByTestId("task-nodes-skip-shared-check")).toContainText(
    "新节点下载的文件会写入其他节点看不到的目录",
  );
  await expect(box.getByTestId("task-nodes-skipped-task")).toContainText(
    "本次添加的节点同样不校验共享目录",
  );
  // Dropping the only added node drops the skip, so it never returns unasked.
  await box.getByRole("button", { name: /^撤销添加 / }).click();
  await expect(joinSkipBox(page)).toHaveCount(0);
  await box.getByRole("button", { name: "添加 Agent", exact: true }).click();
  await page.getByRole("menuitem", { name: /10\.0\.1\.14/ }).click();
  await expect(joinSkipBox(page)).not.toBeChecked();
  await expect(box.getByTestId("task-nodes-skipped-task")).toContainText(
    "本次添加的节点仍会校验共享目录",
  );
  expect(posts).toHaveLength(0);
});
