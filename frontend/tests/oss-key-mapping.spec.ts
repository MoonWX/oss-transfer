import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

async function select(page: Page, label: string, option: string) {
  await page.getByRole("combobox", { name: label }).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}

async function configure(page: Page) {
  await page.goto("/create");
  await select(page, "源存储连接", "production");
  await page.getByRole("combobox", { name: "源存储桶" }).fill("media-archive");
  await select(page, "目标存储连接", "backup");
  await page.getByRole("combobox", { name: "目标存储桶" }).fill("media-backup");
}

const cases = [
  {
    name: "legacy preserve",
    scope: "整个存储桶",
    mode: "保留原 Key",
    expected: {},
  },
  {
    name: "add literal prefix",
    scope: "整个存储桶",
    mode: "添加目标前缀",
    target: " archive//",
    expected: {
      keyMapping: { mode: "ADD_PREFIX", targetPrefix: " archive//" },
    },
  },
  {
    name: "add prefix to single object",
    scope: "单个对象",
    source: "photos// a.jpg",
    mode: "添加目标前缀",
    target: "backup",
    expected: {
      objectKey: "photos// a.jpg",
      keyMapping: { mode: "ADD_PREFIX", targetPrefix: "backup" },
    },
  },
  {
    name: "replace without slash normalization",
    scope: "按对象前缀筛选",
    source: "photos/",
    mode: "替换源前缀",
    target: "archive",
    expected: {
      prefix: "photos/",
      keyMapping: {
        mode: "REPLACE_PREFIX",
        sourcePrefix: "photos/",
        targetPrefix: "archive",
      },
    },
  },
  {
    name: "strip source prefix",
    scope: "按对象前缀筛选",
    source: "photos/",
    mode: "替换源前缀",
    target: "",
    expected: {
      prefix: "photos/",
      keyMapping: {
        mode: "REPLACE_PREFIX",
        sourcePrefix: "photos/",
        targetPrefix: "",
      },
    },
  },
  {
    name: "single exact key",
    scope: "单个对象",
    source: "原图/ a//b.jpg ",
    mode: "指定目标 Key",
    target: "归档/ renamed.jpg ",
    expected: {
      objectKey: "原图/ a//b.jpg ",
      keyMapping: { mode: "EXACT_KEY", targetKey: "归档/ renamed.jpg " },
    },
  },
] as const;

for (const scenario of cases) {
  test(`OSS mapping ${scenario.name} submits the literal request`, async ({
    page,
  }) => {
    const requests = await mockApi(page);
    await configure(page);
    if (scenario.scope !== "整个存储桶") {
      await select(page, "迁移范围", scenario.scope);
      await page
        .getByRole("textbox", {
          name: scenario.scope === "单个对象" ? "源对象 Key" : "源对象前缀",
          exact: true,
        })
        .fill("source" in scenario ? scenario.source : "");
    }
    if (scenario.mode !== "保留原 Key") {
      await select(page, "目标对象命名", scenario.mode);
      await page
        .getByRole("textbox", {
          name:
            scenario.mode === "指定目标 Key" ? "目标对象 Key" : "目标对象前缀",
          exact: true,
        })
        .fill("target" in scenario ? scenario.target : "");
    }
    await page
      .getByRole("button", { name: "创建迁移任务", exact: true })
      .click();
    await expect
      .poll(
        () =>
          requests.filter(
            (request) => request.path === "/api/master/add-buckets",
          ).length,
      )
      .toBe(1);
    const body = requests.find(
      (request) => request.path === "/api/master/add-buckets",
    )!.body[0];
    expect(body).toEqual({
      sourceS3Alias: "production",
      sourceBucket: "media-archive",
      destS3Alias: "backup",
      destBucket: "media-backup",
      distributionMode: "affinity",
      eligibleWorkerNodes: ["10.0.1.12", "10.0.1.13"],
      overwrite: false,
      ...scenario.expected,
    });
  });
}

test("mapping draft survives reload and switching scope clears hidden source and target values", async ({
  page,
}) => {
  // Multiple routes/actions share this total budget; assertions keep their own deadline.
  test.setTimeout(60_000);
  const requests = await mockApi(page);
  await configure(page);
  await select(page, "迁移范围", "单个对象");
  await page
    .getByRole("textbox", { name: "源对象 Key", exact: true })
    .fill("source.txt");
  await select(page, "目标对象命名", "指定目标 Key");
  await page
    .getByRole("textbox", { name: "目标对象 Key", exact: true })
    .fill("renamed.txt");
  await page.reload();
  await expect(
    page.getByRole("textbox", { name: "目标对象 Key", exact: true }),
  ).toHaveValue("renamed.txt");
  await select(page, "迁移范围", "按对象前缀筛选");
  await expect(
    page.getByRole("textbox", { name: "源对象前缀", exact: true }),
  ).toHaveValue("");
  await expect(page.getByRole("combobox", { name: "目标对象命名" })).toHaveText(
    "保留原 Key",
  );
  await page.getByRole("combobox", { name: "目标对象命名" }).click();
  await expect(
    page.getByRole("option", { name: "指定目标 Key", exact: true }),
  ).toHaveCount(0);
  await page.getByRole("option", { name: "替换源前缀", exact: true }).click();
  await page
    .getByRole("textbox", { name: "源对象前缀", exact: true })
    .fill("old/");
  await page
    .getByRole("textbox", { name: "目标对象前缀", exact: true })
    .fill("new/");
  await select(page, "目标对象命名", "保留原 Key");
  await select(page, "目标对象命名", "添加目标前缀");
  await expect(
    page.getByRole("textbox", { name: "目标对象前缀", exact: true }),
  ).toHaveValue("");
  await select(page, "迁移范围", "整个存储桶");
  await select(page, "目标对象命名", "保留原 Key");
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
  await expect
    .poll(
      () =>
        requests.filter((request) => request.path === "/api/master/add-buckets")
          .length,
    )
    .toBe(1);
  const body = requests.find(
    (request) => request.path === "/api/master/add-buckets",
  )!.body[0];
  for (const field of ["objectKey", "prefix", "keyMapping"])
    expect(body).not.toHaveProperty(field);
});

test("JSON mapping rejects incompatible scopes and preserves literal valid bucket and subtask requests", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.goto("/create");
  await page.getByRole("tab", { name: "批量导入" }).click();
  const base = {
    sourceS3Alias: "production",
    sourceBucket: "media-archive",
    destS3Alias: "backup",
    destBucket: "media-backup",
  };
  for (const invalid of [
    { objectKey: "a", prefix: "b/" },
    { keyMapping: { mode: "EXACT_KEY", targetKey: "a" } },
    {
      prefix: "a/",
      keyMapping: {
        mode: "REPLACE_PREFIX",
        sourcePrefix: "b/",
        targetPrefix: "",
      },
    },
    { keyMapping: { mode: "ADD_PREFIX", targetPrefix: "" } },
    { keyMapping: { mode: "PRESERVE", targetKey: "hidden.txt" } },
  ]) {
    await page
      .getByLabel("任务数据")
      .fill(JSON.stringify([{ ...base, ...invalid }]));
    await page.getByRole("button", { name: "导入任务", exact: true }).click();
    await expect(
      page.getByRole("alert").filter({ hasText: "第 1 项：" }),
    ).toBeVisible();
    expect(
      requests.filter((request) =>
        ["/api/master/add-buckets", "/api/master/import"].includes(
          request.path,
        ),
      ),
    ).toHaveLength(0);
  }
  const rows = [
    {
      ...base,
      objectKey: " 空 格//文件 ",
      keyMapping: { mode: "EXACT_KEY", targetKey: "目标// 文件 " },
    },
    {
      ...base,
      prefix: "old/",
      keyMapping: {
        mode: "REPLACE_PREFIX",
        sourcePrefix: "old/",
        targetPrefix: "",
      },
    },
  ];
  for (const kind of ["buckets", "tasks"]) {
    if (kind === "tasks")
      await select(page, "导入类型", "OSS 子任务恢复 / 导入");
    await page.getByLabel("任务数据").fill(JSON.stringify(rows));
    await page.getByRole("button", { name: "导入任务", exact: true }).click();
    await expect(
      page.getByRole("button", { name: "继续配置任务", exact: true }),
    ).toBeVisible();
    expect(
      requests.find(
        (request) =>
          request.path ===
          (kind === "tasks" ? "/api/master/import" : "/api/master/add-buckets"),
      )!.body,
    ).toEqual(rows);
    await page
      .getByRole("button", { name: "继续配置任务", exact: true })
      .click();
  }
});

test("subtask endpoint scopes require an explicit filter and accept literal prefix and object mappings", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.goto("/create");
  await page.getByRole("tab", { name: "批量导入" }).click();
  await select(page, "导入类型", "OSS 子任务恢复 / 导入");
  const base = {
    sourceS3Alias: "production",
    sourceBucket: "media-archive",
    destS3Alias: "backup",
    destBucket: "media-backup",
  };
  for (const extra of [
    { sourceEndpoint: { type: "OSS", prefix: "old/" } },
    {
      sourceEndpoint: {
        type: "OSS",
        prefix: "old/",
        prefixFilterEnabled: false,
      },
    },
    {
      prefix: "new/",
      sourceEndpoint: {
        type: "OSS",
        prefix: "old/",
        prefixFilterEnabled: true,
      },
    },
    { objectKey: "a", sourceEndpoint: { type: "OSS", objectKey: "b" } },
  ]) {
    await page
      .getByLabel("任务数据")
      .fill(JSON.stringify([{ ...base, ...extra }]));
    await page.getByRole("button", { name: "导入任务", exact: true }).click();
    await expect(
      page.getByRole("alert").filter({ hasText: "第 1 项：" }),
    ).toBeVisible();
    expect(
      requests.filter((request) => request.path === "/api/master/import"),
    ).toHaveLength(0);
  }
  const rows = [
    {
      ...base,
      sourceEndpoint: {
        type: "OSS",
        prefix: "old/",
        prefixFilterEnabled: true,
      },
      keyMapping: {
        mode: "REPLACE_PREFIX",
        sourcePrefix: "old/",
        targetPrefix: "new/",
      },
    },
    {
      ...base,
      sourceEndpoint: { type: "OSS", objectKey: "source.txt" },
      keyMapping: { mode: "EXACT_KEY", targetKey: "renamed.txt" },
    },
  ];
  await page.getByLabel("任务数据").fill(JSON.stringify(rows));
  await page.getByRole("button", { name: "导入任务", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "继续配置任务", exact: true }),
  ).toBeVisible();
  expect(
    requests.find((request) => request.path === "/api/master/import")!.body,
  ).toEqual(rows);
});

test("LOCAL upload naming remains independent and retains its existing request shape", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.route("**/api/master/local-transfers/validate", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          valid: true,
          nodeBindings: [
            {
              executionNodeIp: "10.0.1.12",
              localPath: "/mnt/data/source.txt",
              valid: true,
              pathType: "FILE",
            },
          ],
        },
      },
    }),
  );
  await configure(page);
  await select(page, "目标对象命名", "添加目标前缀");
  await page
    .getByRole("textbox", { name: "目标对象前缀", exact: true })
    .fill("oss-only/");
  await page.getByRole("button", { name: /本地 → 对象存储/ }).click();
  await expect(page.getByRole("combobox", { name: "目标对象命名" })).toHaveText(
    "保留源文件名 / 相对路径",
  );
  await select(page, "目标对象命名", "指定单文件目标 Key");
  await page
    .getByRole("textbox", { name: "目标对象 Key", exact: true })
    .fill("local.txt");
  await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
  await page.getByRole("option", { name: /10\.0\.1\.12/ }).click();
  await page.getByLabel("源文件或目录绝对路径").fill("/mnt/data/source.txt");
  await page.getByRole("button", { name: "运行预检", exact: true }).click();
  await expect(page.getByText("节点预检通过", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
  await expect
    .poll(
      () =>
        requests.filter(
          (request) => request.path === "/api/master/local-transfers",
        ).length,
    )
    .toBe(1);
  const body = requests.find(
    (request) => request.path === "/api/master/local-transfers",
  )!.body[0];
  expect(body.destinationEndpoint).toEqual({
    type: "OSS",
    s3Alias: "backup",
    bucket: "media-backup",
    objectKey: "local.txt",
  });
  expect(body).not.toHaveProperty("keyMapping");
});

test("task details and failure records show distinct source and destination keys", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: {
        tasks: [
          {
            ...tasks[0],
            sourceEndpoint: { type: "OSS", prefix: "old/" },
            keyMapping: {
              mode: "REPLACE_PREFIX",
              sourcePrefix: "old/",
              targetPrefix: "new/",
            },
          },
        ],
      },
    }),
  );
  await page.goto("/tasks?taskId=1048");
  await expect(page.getByText("替换源前缀", { exact: true })).toBeVisible();
  await expect(
    page.getByText('"old/example.txt" → "new/example.txt"', { exact: true }),
  ).toBeVisible();
  await page.route("**/api/master/failed**", (route) =>
    route.fulfill({
      json: {
        success: true,
        failedKeys: {
          "10.0.1.12": [
            {
              taskIndex: 1048,
              key: "old/file.txt",
              destinationKey: "new/file.txt",
              errorMessage: "upload failed",
            },
          ],
        },
      },
    }),
  );
  await page.goto("/exceptions");
  await expect(page.getByText("old/file.txt", { exact: true })).toBeVisible();
  await expect(
    page.getByText("目标：new/file.txt", { exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "详情", exact: true }).click();
  await expect(
    page.getByRole("dialog").getByText("源对象 Key", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("dialog").getByText("目标对象 Key", { exact: true }),
  ).toBeVisible();
});

test("mapping controls fit desktop light dark and mobile layouts", async ({
  page,
}, testInfo) => {
  await mockApi(page);
  await page.setViewportSize({ width: 1440, height: 1080 });
  await configure(page);
  await select(page, "迁移范围", "按对象前缀筛选");
  await page
    .getByRole("textbox", { name: "源对象前缀", exact: true })
    .fill("photos/2026/");
  await select(page, "目标对象命名", "替换源前缀");
  await page
    .getByRole("textbox", { name: "目标对象前缀", exact: true })
    .fill("archive/");
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.screenshot({
    path: testInfo.outputPath("desktop-light.png"),
    fullPage: true,
    animations: "disabled",
  });
  await page.getByRole("button", { name: "切换主题" }).click();
  await expect(page.locator("html")).toHaveCSS("color-scheme", "dark");
  await page.screenshot({
    path: testInfo.outputPath("desktop-dark.png"),
    fullPage: true,
    animations: "disabled",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(
    page.getByRole("textbox", { name: "目标对象前缀", exact: true }),
  ).toHaveValue("archive/");
  await expect
    .poll(() => page.evaluate(() => document.documentElement.scrollWidth))
    .toBeLessThanOrEqual(390);
  await page.screenshot({
    path: testInfo.outputPath("mobile-dark.png"),
    fullPage: true,
    animations: "disabled",
  });
});
