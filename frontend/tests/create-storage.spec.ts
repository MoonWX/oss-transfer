import { expect, test, type Page } from "@playwright/test";
import { mockApi, storageConfigs } from "./fixtures";

async function select(page: Page, label: string, option: string) {
  await page.getByRole("combobox", { name: label }).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}

async function fillOssSource(page: Page) {
  await select(page, "源存储连接", "production");
  await page.getByRole("combobox", { name: "源存储桶" }).fill("media-archive");
}

async function fillOssToLocal(page: Page) {
  await page.goto("/create");
  await page.getByRole("button", { name: /对象存储 → 本地/ }).click();
  await fillOssSource(page);
  await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
  await page.getByRole("option", { name: /10\.0\.1\.12/ }).click();
  await page.getByLabel("目标目录绝对路径").fill("/mnt/datasets/media");
}

test("OSS migration submits selected endpoints, scope and advanced options", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.goto("/create");
  await fillOssSource(page);
  await select(page, "目标存储连接", "backup");
  await page.getByRole("combobox", { name: "目标存储桶" }).fill("media-backup");
  await select(page, "迁移范围", "按对象前缀筛选");
  await page.getByLabel("源对象前缀").fill("2026/photos/");
  await page.getByRole("button", { name: /迁移选项/ }).click();
  await select(page, "目标已存在时", "覆盖同名对象或文件");
  await select(page, "节点分发方式", "均衡分发");
  await page.getByLabel("修改时间晚于（可选）").fill("2026-09-01T00:00");
  await page.getByLabel("修改时间早于（可选）").fill("2026-09-22T12:30");
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
  await expect(
    page.getByText("迁移任务已提交", { exact: true }).first(),
  ).toBeVisible();
  const submitted = requests.filter(
    (request) => request.path === "/api/master/add-buckets",
  );
  expect(submitted).toHaveLength(1);
  expect(submitted[0].body).toEqual([
    {
      sourceS3Alias: "production",
      sourceBucket: "media-archive",
      destS3Alias: "backup",
      destBucket: "media-backup",
      prefix: "2026/photos/",
      overwrite: true,
      distributionMode: "balanced",
      eligibleWorkerNodes: ["10.0.1.12", "10.0.1.13"],
      dateAfter: "2026-09-01T00:00:00",
      dateBefore: "2026-09-22T12:30:00",
    },
  ]);
  await expect(
    page.getByRole("button", { name: "继续配置任务" }),
  ).toBeVisible();
  await expect
    .poll(() =>
      page.evaluate(() => sessionStorage.getItem("oss-transfer-create-draft")),
    )
    .toBeNull();
});

test("failed local precheck displays node error and blocks creation", async ({
  page,
}) => {
  const requests = await mockApi(page);
  let validationBody: unknown;
  await page.route("**/api/master/local-transfers/validate", async (route) => {
    validationBody = route.request().postDataJSON();
    await route.fulfill({
      json: {
        success: true,
        data: {
          valid: false,
          nodeBindings: [
            {
              executionNodeIp: "10.0.1.12",
              localPath: "/mnt/datasets/media",
              valid: false,
              code: "LOCAL_PATH_DENIED",
              message: "目标目录不可写",
            },
          ],
        },
      },
    });
  });
  await fillOssToLocal(page);
  await page.getByRole("button", { name: "运行预检" }).click();
  await expect(page.getByText("节点预检未通过", { exact: true })).toBeVisible();
  await expect(page.getByText("目标目录不可写", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("button", { name: "创建迁移任务", exact: true }),
  ).toBeDisabled();
  expect(validationBody).toEqual([
    {
      direction: "OSS_TO_LOCAL",
      conflictPolicy: "SKIP",
      distributionMode: "affinity",
      sourceEndpoint: {
        type: "OSS",
        s3Alias: "production",
        bucket: "media-archive",
      },
      destinationEndpoint: {
        type: "LOCAL",
        nodeBindings: [
          { executionNodeIp: "10.0.1.12", localPath: "/mnt/datasets/media" },
        ],
      },
    },
  ]);
  expect(
    requests.filter(
      (request) => request.path === "/api/master/local-transfers",
    ),
  ).toHaveLength(0);
  await page.getByLabel("目标目录绝对路径").fill("/mnt/datasets/another");
  await expect(
    page.getByRole("button", { name: "创建迁移任务", exact: true }),
  ).toBeDisabled();
  await expect(page.getByText("节点预检未通过", { exact: true })).toHaveCount(
    0,
  );
});

test("single-node download preflight distinguishes a missing destination from an existing directory", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/local-transfers/validate", async (route) => {
    await route.fulfill({
      json: {
        success: true,
        data: {
          valid: true,
          nodeBindings: [
            {
              executionNodeIp: "10.0.1.12",
              localPath: "/mnt/datasets/media",
              valid: true,
              pathType: "MISSING",
              message: "目标目录将在任务开始时创建",
            },
          ],
        },
      },
    });
  });
  await fillOssToLocal(page);
  await page.getByRole("button", { name: "运行预检" }).click();
  await expect(page.getByText("节点预检通过", { exact: true })).toBeVisible();
  await expect(page.getByText("通过 · 目录", { exact: true })).toHaveCount(0);
  await expect(
    page.getByText("通过 · 将创建目录", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("目标目录将在任务开始时创建", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "创建迁移任务", exact: true }),
  ).toBeEnabled();
});

test("shared download shows every missing destination as failed", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.route("**/api/master/local-transfers/validate", async (route) => {
    const bindings = route.request().postDataJSON()[0]
      .destinationEndpoint.nodeBindings;
    await route.fulfill({
      json: {
        success: true,
        data: {
          valid: false,
          nodeBindings: bindings.map((binding: object) => ({
            ...binding,
            valid: false,
            pathType: "MISSING",
            code: "SHARED_DIRECTORY_REQUIRED",
            message: "共享目标目录不存在",
          })),
        },
      },
    });
  });
  await fillOssToLocal(page);
  await page.getByRole("button", { name: "添加共享存储节点" }).click();
  await page
    .getByRole("combobox", { name: "执行节点", exact: true })
    .nth(1)
    .click();
  await page.getByRole("option", { name: /10\.0\.1\.13/ }).click();
  await page.getByLabel("目标目录绝对路径").nth(1).fill("/mnt/datasets/media");
  await page.getByRole("button", { name: "运行预检" }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "节点预检未通过" }),
  ).toContainText("通过 0 / 2 个节点配置。修改配置后需重新预检。");
  await expect(
    page.getByText("共享目标目录不存在", { exact: true }),
  ).toHaveCount(2);
  await expect(page.getByText("未通过", { exact: true })).toHaveCount(2);
  await expect(page.getByText("通过 · 目录", { exact: true })).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "创建迁移任务", exact: true }),
  ).toBeDisabled();
  expect(
    requests.filter(
      (request) => request.path === "/api/master/local-transfers",
    ),
  ).toHaveLength(0);
});

test("shared local migration prechecks and submits all bindings without a storage ID", async ({
  page,
}) => {
  await mockApi(page);
  const validateBodies: unknown[] = [];
  const createBodies: unknown[] = [];
  await page.route("**/api/master/local-transfers/validate", async (route) => {
    const body = route.request().postDataJSON();
    validateBodies.push(body);
    await route.fulfill({
      json: {
        success: true,
        data: {
          valid: true,
          nodeBindings: body[0].destinationEndpoint.nodeBindings.map(
            (binding: object) => ({
              ...binding,
              valid: true,
              pathType: "DIRECTORY",
            }),
          ),
        },
      },
    });
  });
  await page.route("**/api/master/local-transfers", async (route) => {
    const body = route.request().postDataJSON();
    createBodies.push(body);
    await route.fulfill({
      json: {
        success: true,
        data: {
          acceptedCount: 1,
          rejectedCount: 0,
          taskIds: [1049],
          parentTaskIds: [1049],
          acceptedNodeBindings: [
            {
              parentTaskId: 1049,
              distributionMode: body[0].distributionMode,
              nodeBindings: body[0].destinationEndpoint.nodeBindings,
            },
          ],
          validationErrors: [],
        },
      },
    });
  });
  await fillOssToLocal(page);
  await select(page, "迁移范围", "单个对象");
  await page
    .getByLabel("源对象 Key", { exact: false })
    .fill("photos/original.jpg");
  await page.getByRole("button", { name: "添加共享存储节点" }).click();
  await page
    .getByRole("combobox", { name: "执行节点", exact: true })
    .nth(1)
    .click();
  await page.getByRole("option", { name: /10\.0\.1\.13/ }).click();
  await page.getByLabel("目标目录绝对路径").nth(1).fill("/mnt/shared/media");
  await page.evaluate(() => {
    const draft = JSON.parse(
      sessionStorage.getItem("oss-transfer-create-draft")!,
    );
    draft.form.storageId = "legacy-shared-media";
    sessionStorage.setItem("oss-transfer-create-draft", JSON.stringify(draft));
  });
  await page.reload();
  await expect(page.getByLabel("共享存储 ID")).toHaveCount(0);
  await expect(page.getByText(/节点提供的根目录|展示目录/)).toHaveCount(0);
  await page.getByLabel("目标相对路径（可选）").fill("photos/renamed.jpg");
  await page.getByRole("button", { name: /迁移选项/ }).click();
  await select(page, "节点分发方式", "均衡分发");
  await page.getByRole("button", { name: "运行预检" }).click();
  await expect(page.getByText("节点预检通过", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
  await expect(
    page.getByText("迁移任务已提交", { exact: true }).first(),
  ).toBeVisible();
  expect(validateBodies).toHaveLength(1);
  expect(createBodies).toHaveLength(1);
  expect(createBodies[0]).toEqual(validateBodies[0]);
  expect(createBodies[0]).toEqual([
    {
      direction: "OSS_TO_LOCAL",
      conflictPolicy: "SKIP",
      distributionMode: "balanced",
      targetRelativePath: "photos/renamed.jpg",
      sourceEndpoint: {
        type: "OSS",
        s3Alias: "production",
        bucket: "media-archive",
        objectKey: "photos/original.jpg",
      },
      destinationEndpoint: {
        type: "LOCAL",
        nodeBindings: [
          { executionNodeIp: "10.0.1.12", localPath: "/mnt/datasets/media" },
          { executionNodeIp: "10.0.1.13", localPath: "/mnt/shared/media" },
        ],
      },
    },
  ]);
  await expect(
    page.getByText("任务 #1049 · 均衡分发 · 2 个执行节点"),
  ).toBeVisible();
});

for (const nodeCount of [1, 2]) {
  test(`directory upload with ${nodeCount} nodes submits without a storage ID and uses a prefix`, async ({
    page,
  }) => {
    const requests = await mockApi(page);
    await page.goto("/create");
    await page.getByRole("button", { name: /本地 → 对象存储/ }).click();
    await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
    await page.getByRole("option", { name: /10\.0\.1\.12/ }).click();
    await page.getByLabel("源文件或目录绝对路径").fill("/mnt/datasets");
    if (nodeCount === 2) {
      await page.getByRole("button", { name: "添加共享存储节点" }).click();
      await page
        .getByRole("combobox", { name: "执行节点", exact: true })
        .nth(1)
        .click();
      await page.getByRole("option", { name: /10\.0\.1\.13/ }).click();
      await page
        .getByLabel("源文件或目录绝对路径")
        .nth(1)
        .fill("/mnt/shared/datasets");
    }
    await expect(page.getByLabel("共享存储 ID")).toHaveCount(0);
    await select(page, "目标存储连接", "backup");
    await page
      .getByRole("combobox", { name: "目标存储桶" })
      .fill("media-backup");
    await select(page, "目标对象命名", "添加目标前缀");
    await page.getByLabel("目标对象前缀").fill("datasets/2026/");
    await page.getByRole("button", { name: "运行预检", exact: true }).click();
    await page
      .getByRole("button", { name: "创建迁移任务", exact: true })
      .click();
    await expect(
      page.getByText("迁移任务已提交", { exact: true }).first(),
    ).toBeVisible();
    const body = requests.find(
      (request) => request.path === "/api/master/local-transfers",
    )?.body;
    expect(body[0].direction).toBe("LOCAL_TO_OSS");
    expect(body[0].sourceEndpoint).toEqual({
      type: "LOCAL",
      nodeBindings: [
        { executionNodeIp: "10.0.1.12", localPath: "/mnt/datasets" },
        ...(nodeCount === 2
          ? [
              {
                executionNodeIp: "10.0.1.13",
                localPath: "/mnt/shared/datasets",
              },
            ]
          : []),
      ],
    });
    expect(body[0].destinationEndpoint).toEqual({
      type: "OSS",
      s3Alias: "backup",
      bucket: "media-backup",
      prefix: "datasets/2026/",
    });
  });
}

test("batch partial acceptance stays visible and does not claim full success", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/add-buckets", (route) =>
    route.fulfill({ json: { success: true, addedCount: 1 } }),
  );
  await page.goto("/create");
  await page.getByRole("tab", { name: "批量导入" }).click();
  await page.getByLabel("任务数据").fill(
    JSON.stringify([
      { sourceBucket: "bucket-a", destBucket: "bucket-b" },
      { sourceBucket: "bucket-c", destBucket: "bucket-d" },
    ]),
  );
  await page.getByRole("button", { name: "导入任务", exact: true }).click();
  await expect(page.getByText("提交结果待核对", { exact: true })).toBeVisible();
  await expect(page.getByText(/提交 2 个任务，已受理 1 个/)).toBeVisible();
  await expect(page.getByText(/避免重复提交/)).toBeVisible();
  await expect(
    page.getByRole("button", { name: "继续配置任务" }),
  ).toBeVisible();
});

test("continuing a submitted batch preserves its draft without creating duplicate tasks", async ({
  page,
}) => {
  const requests = await mockApi(page);
  const batch = JSON.stringify([
    { sourceBucket: "bucket-a", destBucket: "bucket-b" },
    { sourceBucket: "bucket-c", destBucket: "bucket-d" },
  ]);
  const imports = () =>
    requests.filter((request) => request.path === "/api/master/add-buckets");
  await page.goto("/create");
  await page.getByRole("tab", { name: "批量导入" }).click();
  await page.getByLabel("任务数据").fill(batch);
  await page.getByRole("button", { name: "导入任务", exact: true }).click();
  await expect(page.getByText(/提交 2 个任务，已受理 2 个/)).toBeVisible();
  expect(imports()).toHaveLength(1);

  await page.getByRole("button", { name: "继续配置任务" }).click();
  await expect(page.getByLabel("任务数据")).toHaveValue(batch);
  await page.getByRole("link", { name: "存储连接", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "存储连接", exact: true }),
  ).toBeVisible();
  expect(imports()).toHaveLength(1);
  await page.getByRole("link", { name: "创建迁移", exact: true }).click();
  await expect(page.getByRole("tab", { name: "批量导入" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  await expect(page.getByLabel("任务数据")).toHaveValue(batch);

  const editedBatch = batch.replace("bucket-b", "bucket-next");
  await page.getByLabel("任务数据").fill(editedBatch);
  await page.reload();
  await expect(page.getByLabel("任务数据")).toHaveValue(editedBatch);
  expect(imports()).toHaveLength(1);
  await page.getByRole("button", { name: "导入任务", exact: true }).click();
  await expect(page.getByText(/提交 2 个任务，已受理 2 个/)).toBeVisible();
  expect(imports()).toHaveLength(2);
  expect(imports()[1].body).toEqual(JSON.parse(editedBatch));
});

test("storage CRUD never reloads saved credentials and reports partial worker synchronization", async ({
  page,
}) => {
  await mockApi(page);
  let configs = storageConfigs.map((config) => ({ ...config }));
  const writes: { path: string; method: string; body: any }[] = [];
  await page.route("**/api/master/s3-config**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    const method = request.method();
    if (method === "GET") {
      await route.fulfill({
        json: {
          success: true,
          configs: configs.map(
            ({ accessKey: _ak, secretKey: _sk, ...config }) => config,
          ),
        },
      });
      return;
    }
    const body = request.postDataJSON();
    writes.push({ path, method, body });
    if (method === "POST") configs.push({ ...body });
    if (method === "PUT")
      configs = configs.map((config) =>
        config.alias === path.split("/").at(-1)
          ? { ...config, ...body }
          : config,
      );
    if (method === "DELETE")
      configs = configs.filter(
        (config) => config.alias !== path.split("/").at(-1),
      );
    await route.fulfill({
      json: {
        success: true,
        partial: true,
        successCount: 1,
        failedCount: 1,
        message: `${method === "POST" ? "添加" : method === "PUT" ? "更新" : "删除"}结果：成功 1 个节点，失败 1 个节点`,
        details: [
          `10.0.1.13: ${method === "POST" ? "添加" : method === "PUT" ? "更新" : "删除"}失败`,
        ],
      },
    });
  });
  await page.goto("/storage");
  await expect(page.getByText("TEST_SECRET_KEY", { exact: true })).toHaveCount(
    0,
  );
  await page.getByRole("button", { name: "添加连接", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel("连接名称").fill("new-archive");
  await dialog.getByLabel("Endpoint").fill("https://archive.example.test");
  await dialog.getByLabel("Access Key").fill("NEW_ACCESS_KEY");
  await dialog.getByLabel("Secret Key").fill("NEW_SECRET_KEY");
  await expect(dialog.getByLabel("Access Key")).toHaveAttribute(
    "type",
    "password",
  );
  await expect(dialog.getByLabel("Secret Key")).toHaveAttribute(
    "type",
    "password",
  );
  await expect(
    dialog.getByRole("button", { name: "显示访问凭据" }),
  ).toHaveCount(0);
  await dialog.getByRole("button", { name: "保存连接" }).click();
  await expect(page.getByText("部分节点添加失败")).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "new-archive", exact: true }),
  ).toBeVisible();
  expect(writes[0]).toMatchObject({
    path: "/api/master/s3-config",
    method: "POST",
    body: {
      alias: "new-archive",
      endpoint: "https://archive.example.test",
      accessKey: "NEW_ACCESS_KEY",
      secretKey: "NEW_SECRET_KEY",
      validate: true,
      useV2SignerType: null,
      pathStyleEnable: null,
    },
  });
  await page
    .getByRole("button", { name: "编辑连接 new-archive", exact: true })
    .click();
  await expect(dialog.getByLabel("连接名称")).toBeDisabled();
  await expect(dialog.getByLabel("Secret Key")).toHaveAttribute(
    "type",
    "password",
  );
  await expect(dialog.getByLabel("Access Key")).toHaveValue("");
  await expect(dialog.getByLabel("Secret Key")).toHaveValue("");
  await dialog.getByLabel("Endpoint").fill("https://updated.example.test");
  await dialog.getByRole("button", { name: "保存连接" }).click();
  await expect(
    page.getByText("https://updated.example.test", { exact: true }),
  ).toBeVisible();
  expect(writes[1]).toMatchObject({
    path: "/api/master/s3-config/new-archive",
    method: "PUT",
    body: {
      accessKey: "",
      secretKey: "",
      endpoint: "https://updated.example.test",
    },
  });
  await page
    .getByRole("button", { name: "删除连接 new-archive", exact: true })
    .click();
  await dialog.getByRole("button", { name: "删除连接", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "new-archive", exact: true }),
  ).toHaveCount(0);
  expect(writes[2]).toMatchObject({
    path: "/api/master/s3-config/new-archive",
    method: "DELETE",
  });
});

test("mobile create and storage layouts fit the viewport including dialogs", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await mockApi(page);
  const noOverflow = async () => {
    await expect
      .poll(() =>
        page.evaluate(() => ({
          width: document.documentElement.clientWidth,
          scroll: document.documentElement.scrollWidth,
        })),
      )
      .toEqual({ width: 390, scroll: 390 });
  };
  await fillOssToLocal(page);
  await noOverflow();
  await page.getByRole("button", { name: "添加共享存储节点" }).click();
  await noOverflow();
  await page.getByRole("tab", { name: "批量导入" }).click();
  await page.getByRole("button", { name: "填入示例" }).click();
  await noOverflow();
  await page.goto("/storage");
  await expect(
    page.getByRole("heading", { name: "production", exact: true }),
  ).toBeVisible();
  await noOverflow();
  await page.getByRole("button", { name: "添加连接", exact: true }).click();
  await expect(page.getByRole("dialog")).toBeVisible();
  await noOverflow();
});

test("unsent migration and batch drafts survive adding a storage connection without restoring precheck", async ({
  page,
}) => {
  await mockApi(page);
  let configs = storageConfigs.map((config) => ({ ...config }));
  await page.route("**/api/master/s3-config", async (route) => {
    if (route.request().method() === "POST") {
      configs.push(route.request().postDataJSON());
      await route.fulfill({
        json: { success: true, message: "连接已添加", details: [] },
      });
    } else
      await route.fulfill({
        json: {
          success: true,
          configs: configs.map(
            ({ accessKey: _ak, secretKey: _sk, ...config }) => config,
          ),
        },
      });
  });
  await fillOssToLocal(page);
  await page.getByRole("button", { name: "运行预检" }).click();
  await expect(page.getByText("节点预检通过", { exact: true })).toBeVisible();
  await page.getByRole("link", { name: "存储连接", exact: true }).click();
  await page.getByRole("button", { name: "添加连接", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel("连接名称").fill("draft-destination");
  await dialog.getByLabel("Endpoint").fill("https://draft.example.test");
  await dialog.getByLabel("Access Key").fill("DRAFT_ACCESS_KEY");
  await dialog.getByLabel("Secret Key").fill("DRAFT_SECRET_KEY");
  await dialog.getByRole("button", { name: "保存连接" }).click();
  await expect(
    page.getByRole("heading", {
      name: "draft-destination",
      exact: true,
    }),
  ).toBeVisible();
  await page.getByRole("link", { name: "创建迁移", exact: true }).click();
  await expect(page.getByText(/已恢复当前标签页的草稿/)).toBeVisible();
  await expect(
    page.getByRole("combobox", { name: "源存储连接" }),
  ).toContainText("production");
  await expect(page.getByRole("combobox", { name: "源存储桶" })).toHaveValue(
    "media-archive",
  );
  await expect(
    page.getByRole("combobox", { name: "执行节点", exact: true }),
  ).toHaveValue("master (10.0.1.12)");
  await expect(page.getByLabel("目标目录绝对路径")).toHaveValue(
    "/mnt/datasets/media",
  );
  await expect(page.getByText("节点预检通过", { exact: true })).toHaveCount(0);
  const saved = await page.evaluate(() =>
    sessionStorage.getItem("oss-transfer-create-draft"),
  );
  expect(saved).not.toContain("DRAFT_SECRET_KEY");
  expect(saved).not.toContain("DRAFT_ACCESS_KEY");
  expect(saved).not.toContain("nodeBindings");
  const batch =
    '[{"sourceBucket":"draft-source","destBucket":"draft-destination"}]';
  await page.getByRole("tab", { name: "批量导入" }).click();
  await page.getByLabel("任务数据").fill(batch);
  await page.getByRole("link", { name: "存储连接", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "存储连接", exact: true }),
  ).toBeVisible();
  await page.getByRole("link", { name: "创建迁移", exact: true }).click();
  await expect(page.getByRole("tab", { name: "批量导入" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  await expect(page.getByLabel("任务数据")).toHaveValue(batch);
  await page.getByRole("button", { name: "重置草稿", exact: true }).click();
  await expect(page.getByRole("combobox", { name: "源存储桶" })).toHaveValue(
    "",
  );
  await expect
    .poll(() =>
      page.evaluate(() => sessionStorage.getItem("oss-transfer-create-draft")),
    )
    .toBeNull();
});
