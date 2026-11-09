import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";
import { mockTaskChildren } from "./task-children-fixture";

async function select(page: Page, label: string, option: string) {
  await page.getByRole("combobox", { name: label }).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}

test("the wizard describes the selected conflict policy and submits OVERWRITE_CHANGED", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.goto("/create");
  await select(page, "源存储连接", "production");
  await page.getByRole("combobox", { name: "源存储桶" }).fill("media-archive");
  await select(page, "目标存储连接", "backup");
  await page.getByRole("combobox", { name: "目标存储桶" }).fill("media-backup");
  await page.getByRole("button", { name: /迁移选项/ }).click();
  const policy = page.getByRole("combobox", { name: "目标已存在时" });
  await expect(policy).toHaveAccessibleDescription(
    "目标已有同名对象或文件时直接跳过，不比较大小或内容；源端修改过的对象不会更新。",
  );
  await policy.click();
  await expect(page.getByRole("option")).toHaveText([
    "跳过同名对象或文件",
    "覆盖有变化的对象",
    "覆盖同名对象或文件",
  ]);
  await page.getByRole("option", { name: "覆盖同名对象或文件" }).click();
  await expect(policy).toHaveAccessibleDescription(
    "覆盖会替换目标同名对象或文件，所有对象都会重新传输。",
  );
  await select(page, "目标已存在时", "覆盖有变化的对象");
  await expect(policy).toHaveAccessibleDescription(
    "目标不存在、大小不同、MD5 ETag 不同，或源端修改时间晚于目标时覆盖，其余跳过。不读取对象内容，分片上传的 ETag 不参与比较。",
  );
  await expect(page.getByText(/^覆盖有变化的对象 · /)).toBeVisible();
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
  await expect(
    page.getByText("迁移任务已提交", { exact: true }).first(),
  ).toBeVisible();
  const submitted = requests.filter(
    (request) => request.path === "/api/master/add-buckets",
  );
  expect(submitted).toHaveLength(1);
  expect(submitted[0].body[0]).toMatchObject({
    sourceBucket: "media-archive",
    destBucket: "media-backup",
    overwrite: false,
    conflictPolicy: "OVERWRITE_CHANGED",
  });
});

const finished = {
  ...tasks[0],
  taskIndex: 2100,
  status: "COMPLETED",
  scanComplete: true,
  listObjectCount: 2028,
  putObjectCount: 2,
  skippedObjectCount: 2026,
  failedObjectCount: 0,
  prefix: "2026/",
  keyMapping: { mode: "ADD_PREFIX", targetPrefix: "archive/" },
};

async function openTask(page: Page, task: Record<string, unknown>) {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "0"),
  );
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: 1, tasks: [task] } }),
  );
  await mockTaskChildren(page, () => ({ parentTaskId: 2100, items: [] }));
  await page.goto("/tasks?taskId=2100");
  const panel = page.locator(".MuiDrawer-paper").filter({
    has: page.getByRole("heading", { name: /#2100/ }),
  });
  await expect(panel.getByRole("heading", { name: /#2100/ })).toBeVisible();
  return panel;
}

const note =
  "跳过的对象有两种：目标端已存在的对象只按名称判断，未比较大小或内容，源端修改过的对象在目标端可能仍是旧版本，可用对象比对核对，或使用“覆盖有变化的对象”重新创建任务；扫描后在源端被删除的对象也计为跳过，它们在任务报告中的原因为 SOURCE_DELETED。";

test("a finished SKIP task with skipped objects explains them and links to a prefilled comparison", async ({
  page,
}) => {
  const panel = await openTask(page, { ...finished, conflictPolicy: "SKIP" });
  await expect(
    panel.getByText("跳过同名对象或文件", { exact: true }),
  ).toBeVisible();
  await expect(panel.getByText("SKIP", { exact: true })).toHaveCount(0);
  await expect(panel.getByText(note, { exact: true })).toBeVisible();
  const link = panel.getByRole("link", { name: "对象比对", exact: true });
  await expect(link).toHaveAttribute(
    "href",
    "/tools?sourceBucket=production%2Fmedia-archive&destBucket=backup%2Fmedia-backup&sourcePrefix=2026%2F&destPrefix=archive%2F2026%2F",
  );
  await link.click();
  await expect(page).toHaveURL(/\/tools\?/);
  await expect(page.getByLabel("源存储桶")).toHaveValue(
    "production/media-archive",
  );
  await expect(page.getByLabel("目标存储桶")).toHaveValue(
    "backup/media-backup",
  );
  await expect(page.getByLabel("源前缀")).toHaveValue("2026/");
  await expect(page.getByLabel("目标前缀")).toHaveValue("archive/2026/");
});

for (const [name, task, label] of [
  [
    "an OVERWRITE_CHANGED task",
    { ...finished, conflictPolicy: "OVERWRITE_CHANGED" },
    "覆盖有变化的对象",
  ],
  [
    "a legacy overwrite task",
    { ...finished, conflictPolicy: undefined, overwrite: true },
    "覆盖同名对象或文件",
  ],
  [
    "a SKIP task without skipped objects",
    { ...finished, conflictPolicy: "SKIP", skippedObjectCount: 0 },
    "跳过同名对象或文件",
  ],
  [
    "a running SKIP task",
    { ...finished, conflictPolicy: "SKIP", status: "RUNNING" },
    "跳过同名对象或文件",
  ],
] as const) {
  test(`${name} shows its policy name and no skip note`, async ({ page }) => {
    const panel = await openTask(page, task);
    await expect(panel.getByText(label, { exact: true })).toBeVisible();
    await expect(panel.getByText(note, { exact: true })).toHaveCount(0);
  });
}

test("a local SKIP task explains skipped objects without a comparison link", async ({
  page,
}) => {
  const panel = await openTask(page, {
    ...tasks[2],
    taskIndex: 2100,
    status: "COMPLETED",
    conflictPolicy: "SKIP",
    skippedObjectCount: 3,
    failedObjectCount: 0,
  });
  await expect(panel.getByText(note, { exact: true })).toBeVisible();
  await expect(
    panel.getByRole("link", { name: "对象比对", exact: true }),
  ).toHaveCount(0);
});

test.describe("English", () => {
  test.use({ locale: "en-US" });
  test("policy names and the skip note are translated", async ({ page }) => {
    const panel = await openTask(page, { ...finished, conflictPolicy: "SKIP" });
    await expect(
      panel.getByText("Skip existing objects or files", { exact: true }),
    ).toBeVisible();
    await expect(
      panel.getByText(
        /^Skipped objects are of two kinds\. Objects that already existed at the destination were matched by name only.*deleted at the source after the scan are also counted as skipped.*SOURCE_DELETED\.$/,
      ),
    ).toBeVisible();
    await expect(
      panel.getByRole("link", { name: "Compare objects", exact: true }),
    ).toBeVisible();
  });
});

test.describe("Japanese", () => {
  test.use({ locale: "ja-JP" });
  test("the changed policy is translated", async ({ page }) => {
    const panel = await openTask(page, {
      ...finished,
      conflictPolicy: "OVERWRITE_CHANGED",
    });
    await expect(
      panel.getByText("変更されたオブジェクトを上書き", { exact: true }),
    ).toBeVisible();
  });
});
