import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

async function configureDownload(page: Page) {
  await page.goto("/create");
  await page.getByRole("button", { name: /对象存储 → 本地/ }).click();
  await page.getByRole("combobox", { name: "源存储连接", exact: true }).click();
  await page.getByRole("option", { name: "production", exact: true }).click();
  await page
    .getByRole("combobox", { name: "源存储桶", exact: true })
    .fill("media-archive");
  await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
  await page.getByRole("option", { name: /10\.0\.1\.12/ }).click();
  await page.getByLabel("目标目录绝对路径").fill("/mnt/data/new");
}

test("local creation requires a fresh successful preflight, including form submission", async ({
  page,
}) => {
  const requests = await mockApi(page);
  const validations: any[] = [];
  let nextValid = false;
  let failRequest = false;
  await page.route("**/api/master/local-transfers/validate", async (route) => {
    const body = route.request().postDataJSON();
    validations.push(body);
    if (failRequest) {
      await route.fulfill({
        status: 503,
        json: { success: false, message: "节点暂时不可达" },
      });
      return;
    }
    await route.fulfill({
      json: {
        success: true,
        data: {
          valid: nextValid,
          nodeBindings: body[0].destinationEndpoint.nodeBindings.map(
            (node: object) => ({
              ...node,
              valid: nextValid,
              pathType: nextValid ? "DIRECTORY" : "MISSING",
              message: nextValid ? undefined : "共享目标目录不存在",
            }),
          ),
        },
      },
    });
  });
  await configureDownload(page);
  const create = page.getByRole("button", {
    name: "创建迁移任务",
    exact: true,
  });
  const preflight = page.getByRole("button", { name: "运行预检", exact: true });
  const creations = () =>
    requests.filter(
      (request) => request.path === "/api/master/local-transfers",
    );
  await expect(create).toBeDisabled();
  await page
    .locator("form")
    .evaluate((form) =>
      form.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      ),
    );
  await expect(
    page.getByText("请先通过预检，再创建任务。", { exact: true }).first(),
  ).toBeVisible();
  expect(creations()).toHaveLength(0);
  expect(validations).toHaveLength(0);

  await preflight.click();
  await expect(page.getByText("节点预检未通过", { exact: true })).toBeVisible();
  await expect(create).toBeDisabled();
  nextValid = true;
  await preflight.click();
  await expect(page.getByText("节点预检通过", { exact: true })).toBeVisible();
  await expect(create).toBeEnabled();
  expect(validations).toHaveLength(2);
  expect(validations[0]).toEqual(validations[1]);

  failRequest = true;
  await preflight.click();
  await expect(
    page.getByRole("alert").filter({ hasText: "预检未通过" }),
  ).toContainText("节点暂时不可达");
  await expect(create).toBeDisabled();
  await expect(page.getByText("节点预检通过", { exact: true })).toHaveCount(0);
  failRequest = false;
  await preflight.click();
  await expect(create).toBeEnabled();

  await page.getByLabel("目标目录绝对路径").fill("/mnt/data/changed");
  await expect(create).toBeDisabled();
  await page
    .locator("form")
    .evaluate((form) =>
      form.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      ),
    );
  expect(creations()).toHaveLength(0);
  await preflight.click();
  await expect(create).toBeEnabled();
  await create.click();
  await expect.poll(() => creations().length).toBe(1);
  expect(
    creations()[0].body[0].destinationEndpoint.nodeBindings[0].localPath,
  ).toBe("/mnt/data/changed");
  expect(validations).toHaveLength(5);
});

test("batch local preflight expires when the input changes or the draft reloads", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.goto("/create");
  await page.getByRole("tab", { name: "批量导入", exact: true }).click();
  await page.getByRole("combobox", { name: "导入类型" }).click();
  await page.getByRole("option", { name: "本地迁移请求", exact: true }).click();
  const rows = [
    {
      direction: "OSS_TO_LOCAL",
      sourceEndpoint: {
        type: "OSS",
        s3Alias: "production",
        bucket: "media-archive",
      },
      destinationEndpoint: {
        type: "LOCAL",
        nodeBindings: [
          { executionNodeIp: "10.0.1.12", localPath: "/mnt/data/first" },
        ],
      },
    },
  ];
  await page.getByLabel("任务数据", { exact: true }).fill(JSON.stringify(rows));
  const create = page.getByRole("button", { name: "导入任务", exact: true });
  const preflight = page.getByRole("button", { name: "运行预检", exact: true });
  await expect(create).toBeDisabled();
  await preflight.click();
  await expect(create).toBeEnabled();
  rows[0].destinationEndpoint.nodeBindings[0].localPath = "/mnt/data/second";
  await page.getByLabel("任务数据", { exact: true }).fill(JSON.stringify(rows));
  await expect(create).toBeDisabled();
  await preflight.click();
  await expect(create).toBeEnabled();
  await page.reload();
  await expect(create).toBeDisabled();
  await expect(page.getByText("节点预检通过", { exact: true })).toHaveCount(0);
  await preflight.click();
  await expect(create).toBeEnabled();
  await create.click();
  await expect
    .poll(
      () =>
        requests.filter(
          (request) => request.path === "/api/master/local-transfers",
        ).length,
    )
    .toBe(1);
});

test("creation rejection invalidates earlier preflight without changing the retry identity", async ({
  page,
}) => {
  await mockApi(page);
  const keys: string[] = [];
  await page.route("**/api/master/local-transfers", async (route) => {
    keys.push(route.request().headers()["idempotency-key"]);
    const rejected = keys.length === 1;
    await route.fulfill({
      json: {
        success: true,
        data: {
          acceptedCount: rejected ? 0 : 1,
          rejectedCount: rejected ? 1 : 0,
          partial: rejected,
          parentTaskIds: rejected ? [] : [1049],
          validationErrors: rejected ? ["共享目标目录不存在"] : [],
        },
      },
    });
  });
  await configureDownload(page);
  const preflight = page.getByRole("button", { name: "运行预检", exact: true });
  const create = page.getByRole("button", {
    name: "创建迁移任务",
    exact: true,
  });
  await preflight.click();
  await expect(page.getByText("节点预检通过", { exact: true })).toBeVisible();
  await create.click();
  await expect(
    page.getByText("共享目标目录不存在", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText("节点预检通过", { exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "继续配置任务", exact: true }).click();
  await expect(create).toBeDisabled();
  await preflight.click();
  await create.click();
  await expect(
    page.getByText("迁移任务已提交", { exact: true }).first(),
  ).toBeVisible();
  expect(keys).toHaveLength(2);
  expect(keys[0]).toBeTruthy();
  expect(keys[1]).toBe(keys[0]);
});
