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
    validations.push(route.request().postDataJSON()[0]);
    return route.fulfill({
      json: {
        success: true,
        data: {
          valid: true,
          nodeBindings: nodes.map((executionNodeIp) => ({
            executionNodeIp,
            localPath: "/mnt/datasets/qa",
            valid: true,
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
    "节点失联后，自动接管可能在另一个目录上继续执行。",
  ])
    await expect(dialog(page).getByText(consequence)).toBeVisible();
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
  await expect(page.getByText("所有节点预检通过").first()).toBeVisible();
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
  await page.goto("/tasks?taskId=1048");
  await expect(page.getByText("执行信息", { exact: true })).toBeVisible();
  await expect(page.getByTestId("task-shared-check-skipped")).toHaveCount(0);
});
