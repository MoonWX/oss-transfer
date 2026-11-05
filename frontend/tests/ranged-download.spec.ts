import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

type Direction = "OSS_TO_OSS" | "OSS_TO_LOCAL" | "LOCAL_TO_OSS";

async function select(page: Page, label: string, option: string) {
  // A select's accessible name also contains its current value.
  await page.getByRole("combobox", { name: label }).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}
async function chooseDirection(page: Page, direction: Direction) {
  await page
    .getByRole("button", {
      name:
        direction === "OSS_TO_LOCAL"
          ? /对象存储 → 本地/
          : direction === "LOCAL_TO_OSS"
            ? /本地 → 对象存储/
            : /对象存储 → 对象存储/,
    })
    .click();
}
async function configure(page: Page, direction: Direction) {
  await page.goto("/create");
  await chooseDirection(page, direction);
  if (direction !== "LOCAL_TO_OSS") {
    await select(page, "源存储连接", "production");
    await page
      .getByRole("combobox", { name: "源存储桶" })
      .fill("media-archive");
  }
  if (direction !== "OSS_TO_LOCAL") {
    await select(page, "目标存储连接", "backup");
    await page
      .getByRole("combobox", { name: "目标存储桶" })
      .fill("media-backup");
  }
  if (direction !== "OSS_TO_OSS") {
    await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
    await page.getByRole("option", { name: /10\.0\.1\.12/ }).click();
    await page
      .getByLabel(
        direction === "OSS_TO_LOCAL"
          ? "目标目录绝对路径"
          : "源文件或目录绝对路径",
      )
      .fill("/mnt/datasets/qa");
  }
  await page.getByRole("button", { name: /迁移选项/ }).click();
}
const rangedSelect = (page: Page) =>
  page.getByRole("combobox", { name: "分段下载" });
const creationPath = (direction: Direction) =>
  direction === "OSS_TO_OSS"
    ? "/api/master/add-buckets"
    : "/api/master/local-transfers";

async function createAndCollect(
  page: Page,
  requests: Awaited<ReturnType<typeof mockApi>>,
  direction: Direction,
) {
  if (direction !== "OSS_TO_OSS") {
    await page.getByRole("button", { name: "运行预检", exact: true }).click();
    await expect(page.getByText("所有节点预检通过").first()).toBeVisible();
  }
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
  const path = creationPath(direction);
  await expect
    .poll(
      () =>
        requests.filter((r) => r.path === path && r.method === "POST").length,
    )
    .toBe(1);
  return requests
    .filter(
      (r) =>
        (r.path === path && r.method === "POST") ||
        r.path === "/api/master/local-transfers/validate",
    )
    .map((r) => r.body[0]);
}

test("downloads to a local directory send rangedDownload only when it is turned off", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await configure(page, "OSS_TO_LOCAL");
  await expect(rangedSelect(page)).toContainText("自动（默认）");
  await expect(
    page.getByText(/^自动适合绝大多数场景：128 MiB 及以上的对象/),
  ).toBeVisible();
  await select(page, "分段下载", "停用");
  await expect(rangedSelect(page)).toContainText("停用");
  await expect(
    page.getByText("跳过同名对象或文件 · 均衡分发 · 分段下载已停用", {
      exact: true,
    }),
  ).toBeVisible();
  const bodies = await createAndCollect(page, requests, "OSS_TO_LOCAL");
  expect(bodies).toHaveLength(2);
  for (const body of bodies) expect(body.rangedDownload).toBe("off");
});

for (const direction of [
  "OSS_TO_LOCAL",
  "OSS_TO_OSS",
  "LOCAL_TO_OSS",
] as Direction[]) {
  test(`${direction} omits rangedDownload by default`, async ({ page }) => {
    const requests = await mockApi(page);
    await configure(page, direction);
    await expect(rangedSelect(page)).toHaveCount(
      direction === "OSS_TO_LOCAL" ? 1 : 0,
    );
    const bodies = await createAndCollect(page, requests, direction);
    expect(bodies.length).toBeGreaterThan(0);
    for (const body of bodies)
      expect(body).not.toHaveProperty("rangedDownload");
  });
}

test("switching away from a local destination drops the off choice", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await configure(page, "OSS_TO_OSS");
  await chooseDirection(page, "OSS_TO_LOCAL");
  await select(page, "分段下载", "停用");
  await chooseDirection(page, "OSS_TO_OSS");
  await expect(rangedSelect(page)).toHaveCount(0);
  const [body] = await createAndCollect(page, requests, "OSS_TO_OSS");
  expect(body).not.toHaveProperty("rangedDownload");
  await page.getByRole("button", { name: "继续配置任务", exact: true }).click();
  await chooseDirection(page, "OSS_TO_LOCAL");
  await expect(rangedSelect(page)).toContainText("自动（默认）");
});

test("the off choice survives a draft reload", async ({ page }) => {
  await mockApi(page);
  await configure(page, "OSS_TO_LOCAL");
  await select(page, "分段下载", "停用");
  await page.reload();
  await page.getByRole("button", { name: /迁移选项/ }).click();
  await expect(rangedSelect(page)).toContainText("停用");
});

test("batch import passes rangedDownload through and rejects it where it cannot apply", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.goto("/create");
  await page.getByRole("tab", { name: "批量导入", exact: true }).click();
  const oss = { type: "OSS", s3Alias: "production", bucket: "media-archive" };
  const local = {
    type: "LOCAL",
    nodeBindings: [{ executionNodeIp: "10.0.1.12", localPath: "/mnt/data" }],
  };
  const download = {
    direction: "OSS_TO_LOCAL",
    sourceEndpoint: oss,
    destinationEndpoint: local,
  };
  const upload = {
    direction: "LOCAL_TO_OSS",
    sourceEndpoint: local,
    destinationEndpoint: oss,
  };
  const data = page.getByLabel("任务数据", { exact: true });
  const bucket = {
    sourceS3Alias: "production",
    sourceBucket: "media-archive",
    destS3Alias: "backup",
    destBucket: "media-backup",
  };
  await data.fill(JSON.stringify([{ ...bucket, rangedDownload: "off" }]));
  await page.getByRole("button", { name: "导入任务", exact: true }).click();
  const alert = page.getByRole("alert").filter({ hasText: "任务配置无效" });
  await expect(alert).toContainText(
    "第 1 项：仅目标为本地目录（OSS_TO_LOCAL）的任务可以设置 rangedDownload: off。",
  );
  expect(
    requests.filter((r) => r.path === "/api/master/add-buckets"),
  ).toHaveLength(0);

  await select(page, "导入类型", "本地迁移请求");
  await data.fill(
    JSON.stringify([download, { ...upload, rangedDownload: "off" }]),
  );
  await page.getByRole("button", { name: "运行预检", exact: true }).click();
  await expect(alert).toContainText(
    "第 2 项：仅目标为本地目录（OSS_TO_LOCAL）的任务可以设置 rangedDownload: off。",
  );
  await data.fill(JSON.stringify([{ ...download, rangedDownload: "OFF" }]));
  await page.getByRole("button", { name: "运行预检", exact: true }).click();
  await expect(alert).toContainText(
    "第 1 项：rangedDownload 必须是 auto 或 off。",
  );
  expect(
    requests.filter((r) => r.path.startsWith("/api/master/local-transfers")),
  ).toHaveLength(0);

  const rows = [
    { ...download, rangedDownload: "off" },
    { ...download, rangedDownload: "auto" },
    download,
    { ...upload, rangedDownload: "auto" },
  ];
  await data.fill(JSON.stringify(rows));
  await page.getByRole("button", { name: "运行预检", exact: true }).click();
  await page.getByRole("button", { name: "导入任务", exact: true }).click();
  const creation = () =>
    requests.find(
      (r) => r.path === "/api/master/local-transfers" && r.method === "POST",
    );
  await expect.poll(creation).toBeTruthy();
  expect(creation()!.body.map((row: any) => row.rangedDownload)).toEqual([
    "off",
    "auto",
    undefined,
    "auto",
  ]);
});

async function showTasks(page: Page, extra: Record<number, object>) {
  await mockApi(page);
  const rows = tasks.map((task) => ({ ...task, ...extra[task.taskIndex] }));
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: { success: true, totalTasks: rows.length, tasks: rows },
    }),
  );
}
const detailValue = (page: Page, label: string) =>
  page
    .getByText("执行信息", { exact: true })
    .locator("..")
    .getByText(label, { exact: true })
    .locator("..");

test("task details show the ranged download choice for local destinations only", async ({
  page,
}) => {
  await showTasks(page, { 1046: { rangedDownload: "off" } });
  await page.goto("/tasks?taskId=1046");
  await expect(detailValue(page, "分段下载")).toHaveText("分段下载停用");
  await page.goto("/tasks?taskId=1048");
  await expect(page.getByText("执行信息", { exact: true })).toBeVisible();
  await expect(page.getByText("分段下载", { exact: true })).toHaveCount(0);
});

test("older local download tasks without the field show auto", async ({
  page,
}) => {
  await showTasks(page, {});
  await page.goto("/tasks?taskId=1046");
  await expect(detailValue(page, "分段下载")).toHaveText("分段下载自动");
});

for (const [locale, label, value] of [
  ["en-US", "Ranged download", "Off"],
  ["ja-JP", "範囲分割ダウンロード", "オフ"],
] as const) {
  test.describe(`${locale} labels`, () => {
    test.use({ locale });
    test("task details translate the ranged download choice", async ({
      page,
    }) => {
      await showTasks(page, { 1046: { rangedDownload: "off" } });
      await page.goto("/tasks?taskId=1046");
      await expect(
        page.getByText(label, { exact: true }).locator(".."),
      ).toHaveText(`${label}${value}`);
    });
  });
}
