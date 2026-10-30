import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

async function select(page: Page, label: string, option: string) {
  await page.getByRole("combobox", { name: label }).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}
async function configureUpload(
  page: Page,
  nodeCount: number,
  savedMode = "affinity",
) {
  await page.goto("/create");
  await page.getByRole("button", { name: /迁移选项/ }).click();
  await select(
    page,
    "节点分发方式",
    savedMode === "balanced" ? "均衡分发" : "亲和分发",
  );
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
  await select(page, "目标存储连接", "backup");
  await page.getByRole("combobox", { name: "目标存储桶" }).fill("media-backup");
}
async function uploadApi(
  page: Page,
  pathType = "DIRECTORY",
  rejectedByServer = false,
) {
  const validationBodies: any[] = [],
    creationBodies: any[] = [];
  await page.route("**/api/master/local-transfers/validate", (route) => {
    const body = route.request().postDataJSON();
    validationBodies.push(body);
    return route.fulfill({
      json: {
        success: true,
        data: {
          valid: !rejectedByServer,
          nodeBindings: body[0].sourceEndpoint.nodeBindings.map(
            (node: object) => ({
              ...node,
              valid: !rejectedByServer,
              pathType,
              ...(rejectedByServer
                ? {
                    code: "SINGLE_FILE_REQUIRES_ONE_NODE",
                    message: "单文件上传请只保留一个执行节点。",
                  }
                : {}),
            }),
          ),
        },
      },
    });
  });
  await page.route("**/api/master/local-transfers", (route) => {
    const body = route.request().postDataJSON();
    creationBodies.push(body);
    return route.fulfill({
      json: {
        success: true,
        data: {
          acceptedCount: body.length,
          parentTaskIds: body.map((_: object, index: number) => 1200 + index),
          acceptedNodeBindings: body.map((row: any, index: number) => ({
            parentTaskId: 1200 + index,
            distributionMode: row.distributionMode,
            nodeBindings: row.sourceEndpoint?.nodeBindings ||
              row.destinationEndpoint?.nodeBindings || [
                { executionNodeIp: row.sourceEndpoint?.executionNodeIp },
              ],
          })),
        },
      },
    });
  });
  return { validationBodies, creationBodies };
}

for (const nodeCount of [1, 2]) {
  test(`${nodeCount} local upload nodes derive scheduling independently of the saved manual mode`, async ({
    page,
  }) => {
    await mockApi(page);
    const calls = await uploadApi(page);
    await configureUpload(
      page,
      nodeCount,
      nodeCount === 1 ? "balanced" : "affinity",
    );
    await expect(
      page.getByRole("combobox", { name: "节点分发方式" }),
    ).toHaveCount(0);
    await expect(
      page.getByText(
        `跳过同名对象或文件 · ${nodeCount === 1 ? "单节点执行" : "均衡分发"}`,
        { exact: true },
      ),
    ).toBeVisible();
    await page.getByRole("button", { name: "运行预检", exact: true }).click();
    await expect(page.getByText("节点预检通过", { exact: true })).toBeVisible();
    await page
      .getByRole("button", { name: "创建迁移任务", exact: true })
      .click();
    await expect.poll(() => calls.creationBodies.length).toBe(1);
    for (const body of [...calls.validationBodies, ...calls.creationBodies])
      expect(body[0].distributionMode).toBe(
        nodeCount === 1 ? "affinity" : "balanced",
      );
    await expect(
      page.getByText(
        `任务 #1200 · ${nodeCount === 1 ? "单节点执行" : "均衡分发"} · ${nodeCount} 个执行节点`,
        { exact: true },
      ),
    ).toBeVisible();
  });
}

for (const savedMode of ["affinity", "balanced"]) {
  test(`direction switching and draft reload retain ${savedMode} only for manually configured directions`, async ({
    page,
  }) => {
    await mockApi(page);
    await configureUpload(page, savedMode === "affinity" ? 2 : 1, savedMode);
    await page.reload();
    await page.getByRole("button", { name: /迁移选项/ }).click();
    await expect(
      page.getByRole("combobox", { name: "节点分发方式" }),
    ).toHaveCount(0);
    await expect(
      page.getByText(
        `跳过同名对象或文件 · ${savedMode === "affinity" ? "均衡分发" : "单节点执行"}`,
        { exact: true },
      ),
    ).toBeVisible();
    for (const direction of [/对象存储 → 本地/, /对象存储 → 对象存储/]) {
      await page.getByRole("button", { name: direction }).click();
      await expect(
        page.getByRole("combobox", { name: "节点分发方式" }),
      ).toContainText(savedMode === "balanced" ? "均衡分发" : "亲和分发");
    }
  });
}

for (const manualPrecheck of [true, false]) {
  test(`multi-node single-file upload is blocked ${manualPrecheck ? "by manual precheck" : "before direct creation"} without changing chosen nodes`, async ({
    page,
  }) => {
    await mockApi(page);
    const calls = await uploadApi(page, "FILE");
    await configureUpload(page, 2);
    await page
      .getByRole("button", {
        name: manualPrecheck ? "运行预检" : "创建迁移任务",
        exact: true,
      })
      .click();
    await expect(
      page.getByText("节点预检未通过", { exact: true }),
    ).toBeVisible();
    await expect(
      page
        .getByText("单文件上传请只保留一个执行节点。", { exact: true })
        .first(),
    ).toBeVisible();
    await expect(
      page.getByRole("combobox", { name: "执行节点", exact: true }),
    ).toHaveCount(2);
    await expect(
      page.getByRole("button", { name: "创建迁移任务", exact: true }),
    ).toBeDisabled();
    expect(calls.creationBodies).toHaveLength(0);
    await page
      .getByRole("button", { name: "移除执行节点 1", exact: true })
      .click();
    await expect(
      page.getByRole("combobox", { name: "执行节点", exact: true }),
    ).toHaveValue("agent (10.0.1.13)");
    await expect(
      page.getByText("跳过同名对象或文件 · 单节点执行", { exact: true }),
    ).toBeVisible();
    await page
      .getByRole("button", { name: "创建迁移任务", exact: true })
      .click();
    await expect.poll(() => calls.creationBodies.length).toBe(1);
    expect(calls.creationBodies[0][0].sourceEndpoint.nodeBindings).toEqual([
      { executionNodeIp: "10.0.1.13", localPath: "/mnt/shared/datasets" },
    ]);
    expect(calls.creationBodies[0][0].distributionMode).toBe("affinity");
  });
}

test("server single-file rejection is retained as a failed precheck", async ({
  page,
}) => {
  await mockApi(page);
  const calls = await uploadApi(page, "FILE", true);
  await configureUpload(page, 2);
  await page.getByRole("button", { name: "运行预检", exact: true }).click();
  await expect(page.getByText("节点预检未通过", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("button", { name: "创建迁移任务", exact: true }),
  ).toBeDisabled();
  expect(calls.creationBodies).toHaveLength(0);
});

test("local JSON batch normalizes upload modes while preserving download modes and legacy single-node input", async ({
  page,
}) => {
  await mockApi(page);
  const calls = await uploadApi(page);
  await page.goto("/create");
  await page.getByRole("tab", { name: "批量导入", exact: true }).click();
  await select(page, "导入类型", "本地迁移请求");
  const bindings = [
    { executionNodeIp: "10.0.1.12", localPath: "/mnt/datasets" },
    { executionNodeIp: "10.0.1.13", localPath: "/mnt/shared/datasets" },
  ];
  const destinationEndpoint = {
    type: "OSS",
    s3Alias: "backup",
    bucket: "media-backup",
  };
  const rows = [
    {
      direction: "LOCAL_TO_OSS",
      distributionMode: "balanced",
      sourceEndpoint: { type: "LOCAL", nodeBindings: bindings.slice(0, 1) },
      destinationEndpoint,
    },
    {
      direction: "LOCAL_TO_OSS",
      distributionMode: "affinity",
      sourceEndpoint: { type: "LOCAL", nodeBindings: bindings },
      destinationEndpoint,
    },
    {
      direction: "LOCAL_TO_OSS",
      distributionMode: "balanced",
      sourceEndpoint: { type: "LOCAL", ...bindings[0] },
      destinationEndpoint,
    },
    {
      direction: "OSS_TO_LOCAL",
      distributionMode: "affinity",
      sourceEndpoint: destinationEndpoint,
      destinationEndpoint: { type: "LOCAL", nodeBindings: bindings },
    },
  ];
  await page.getByLabel("任务数据", { exact: true }).fill(JSON.stringify(rows));
  await page.getByRole("button", { name: "导入任务", exact: true }).click();
  await expect.poll(() => calls.creationBodies.length).toBe(1);
  expect(
    calls.creationBodies[0].map((row: any) => row.distributionMode),
  ).toEqual(["affinity", "balanced", "affinity", "affinity"]);
  expect(calls.creationBodies[0].map((row: any) => row.sourceEndpoint)).toEqual(
    rows.map((row) => row.sourceEndpoint),
  );
});

test("local batch examples use computed upload scheduling", async ({
  page,
}) => {
  await mockApi(page);
  await page.goto("/create");
  await page.getByRole("tab", { name: "批量导入", exact: true }).click();
  await select(page, "导入类型", "本地迁移请求");
  await page.getByRole("button", { name: "填入示例", exact: true }).click();
  const examples = JSON.parse(
    await page.getByLabel("任务数据", { exact: true }).inputValue(),
  );
  const upload = examples.find((row: any) => row.direction === "LOCAL_TO_OSS");
  expect(upload.sourceEndpoint.nodeBindings).toHaveLength(2);
  expect(upload.distributionMode).toBe("balanced");
});

test("automatic upload scheduling fits desktop and mobile with no redundant selector", async ({
  page,
}, testInfo) => {
  await mockApi(page);
  await configureUpload(page, 2);
  for (const width of [390, 1440]) {
    await page.setViewportSize({ width, height: 1000 });
    await page
      .getByRole("button", { name: "创建迁移任务", exact: true })
      .scrollIntoViewIfNeeded();
    await expect(
      page.getByText("跳过同名对象或文件 · 均衡分发", { exact: true }),
    ).toBeVisible();
    await expect(
      page.getByRole("combobox", { name: "节点分发方式" }),
    ).toHaveCount(0);
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth - innerWidth,
      ),
    ).toBeLessThanOrEqual(1);
    await page.screenshot({
      path: testInfo.outputPath(`local-upload-auto-mode-${width}.png`),
      animations: "disabled",
    });
  }
});

test("a delayed precheck locks creation tabs until its result applies to the original form", async ({
  page,
}) => {
  await mockApi(page);
  let release!: () => void;
  const responseReady = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route("**/api/master/local-transfers/validate", async (route) => {
    const body = route.request().postDataJSON();
    await responseReady;
    await route.fulfill({
      json: {
        success: true,
        data: {
          valid: true,
          nodeBindings: body[0].sourceEndpoint.nodeBindings.map(
            (node: object) => ({ ...node, valid: true, pathType: "FILE" }),
          ),
        },
      },
    });
  });
  await configureUpload(page, 2);
  await page.getByRole("button", { name: "运行预检", exact: true }).click();
  const manualTab = page.getByRole("tab", { name: "配置迁移", exact: true });
  const batchTab = page.getByRole("tab", { name: "批量导入", exact: true });
  await expect(manualTab).toBeDisabled();
  await expect(batchTab).toBeDisabled();
  await batchTab.click({ force: true });
  await expect(manualTab).toHaveAttribute("aria-selected", "true");
  await expect(batchTab).toHaveAttribute("aria-selected", "false");
  release();
  await expect(page.getByText("节点预检未通过", { exact: true })).toBeVisible();
  await expect(manualTab).toBeEnabled();
  await expect(batchTab).toBeEnabled();
  await expect(
    page.getByRole("button", { name: "创建迁移任务", exact: true }),
  ).toBeDisabled();
  await batchTab.click();
  await expect(batchTab).toHaveAttribute("aria-selected", "true");
  await expect(page.getByText("节点预检未通过", { exact: true })).toHaveCount(
    0,
  );
});
