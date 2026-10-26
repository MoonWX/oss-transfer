import { expect, test, type Page } from "@playwright/test";
import { mockApi, storageConfigs, tasks } from "./fixtures";

async function select(page: Page, label: string, option: string) {
  await page.getByRole("combobox", { name: label }).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}

async function fillSource(page: Page) {
  await select(page, "源存储连接", "production");
  await page.getByRole("combobox", { name: "源存储桶" }).fill("source-bucket");
}

async function fillOss(page: Page) {
  await page.goto("/create");
  await fillSource(page);
  await select(page, "目标存储连接", "backup");
  await page
    .getByRole("combobox", { name: "目标存储桶" })
    .fill("target-bucket");
}

const directions = [
  { value: "OSS_TO_OSS", label: "OSS → OSS" },
  { value: "OSS_TO_LOCAL", label: "OSS → 本地" },
  { value: "LOCAL_TO_OSS", label: "本地 → OSS" },
];
const states = [
  {
    value: "STOPPING",
    label: "停止中",
    stop: false,
    resume: false,
    remove: false,
  },
  {
    value: "RUNNING",
    label: "执行中",
    stop: true,
    resume: false,
    remove: false,
  },
  {
    value: "RESUMING",
    label: "恢复中",
    stop: true,
    resume: false,
    remove: false,
  },
  {
    value: "COMPLETED",
    label: "已完成",
    stop: false,
    resume: false,
    remove: true,
  },
  {
    value: "PARTIAL_FAILED",
    label: "部分失败",
    stop: false,
    resume: true,
    remove: true,
  },
  { value: "FAILED", label: "失败", stop: false, resume: true, remove: true },
  {
    value: "STOPPED",
    label: "已停止",
    stop: false,
    resume: true,
    remove: true,
  },
];

// Each combination has its own time budget and can run in a separate CI shard.
for (const [i, state] of states.entries()) {
  for (const [j, direction] of directions.entries()) {
    test(`task ${state.value} ${direction.value} filters participating nodes and preserves action eligibility`, async ({
      page,
    }) => {
      const requests = await mockApi(page);
      const matrix = states.flatMap((state, i) =>
        directions.map((direction, j) => ({
          ...tasks[0],
          taskIndex: 2000 + i * 3 + j,
          status: state.value,
          direction: direction.value,
          participatingWorkerNodes:
            j === 0 ? ["10.0.1.12"] : ["10.0.1.12", "10.0.1.13"],
        })),
      );
      await page.route("**/api/master/status", (route) =>
        route.fulfill({ json: { totalTasks: matrix.length, tasks: matrix } }),
      );
      await page.goto("/tasks");
      const table = page.getByRole("table", { name: "迁移任务列表" });
      await select(page, "任务状态", state.label);
      await select(page, "传输方向", direction.label);
      await select(page, "执行节点", "master");
      const id = 2000 + i * 3 + j;
      await expect(table.getByRole("row")).toHaveCount(2);
      await expect(
        table.getByRole("button", { name: `#${id}`, exact: true }),
      ).toBeVisible();
      await page
        .getByRole("checkbox", { name: `选择任务 ${id}`, exact: true })
        .check();
      await expect(
        page.getByRole("button", { name: "启动", exact: true }),
      ).toBeEnabled({ enabled: false });
      await expect(
        page.getByRole("button", { name: "停止", exact: true }),
      ).toBeEnabled({ enabled: state.stop });
      await expect(
        page.getByRole("button", { name: "继续传输", exact: true }),
      ).toBeEnabled({ enabled: state.resume });
      await expect(
        page.getByRole("button", { name: "删除", exact: true }),
      ).toBeEnabled({ enabled: state.remove });
      await select(page, "执行节点", "agent");
      await expect(
        page.getByRole("button", { name: "取消选择", exact: true }),
      ).toHaveCount(0);
      if (j === 0)
        await expect(
          page.getByText("没有匹配的任务", { exact: true }),
        ).toBeVisible();
      else
        await expect(
          table.getByRole("button", { name: `#${id}`, exact: true }),
        ).toBeVisible();
      await select(page, "任务状态", "全部状态");
      await select(page, "传输方向", "全部方向");
      await page
        .getByRole("textbox", { name: "搜索任务", exact: true })
        .fill("not-a-task");
      await expect(
        page.getByText("没有匹配的任务", { exact: true }),
      ).toBeVisible();
      expect(
        requests.filter((request) => request.method !== "GET"),
      ).toHaveLength(0);
    });
  }
}

test("pagination and all-results selection cannot leak hidden task IDs into an action after filtering", async ({
  page,
}) => {
  // Pagination, page size, filtering and confirmation share this total budget.
  test.setTimeout(60_000);
  const requests = await mockApi(page);
  const matrix = Array.from({ length: 27 }, (_, i) => ({
    ...tasks[1],
    taskIndex: 2100 + i,
    generation: 1,
    sourceBucket: `source-${i}`,
    workerNode: i % 2 ? "10.0.1.13" : "10.0.1.12",
  }));
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: matrix.length, tasks: matrix } }),
  );
  await page.route(/\/api\/master\/tasks\/\d+$/, (route) => {
    const id = Number(new URL(route.request().url()).pathname.split("/").pop());
    return route.fulfill({
      json: {
        success: true,
        data: matrix.find((task) => task.taskIndex === id),
      },
    });
  });
  await page.goto("/tasks");
  await page.getByRole("checkbox", { name: "选择当前页任务" }).check();
  await expect(page.getByText("已选 10 项", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "下一页", exact: true }).click();
  await expect(page.getByText("11–20 / 27", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("checkbox", { name: "选择当前页任务" }),
  ).not.toBeChecked();
  await page
    .getByRole("button", { name: "选择全部 27 个结果", exact: true })
    .click();
  await expect(page.getByText("已选 27 项", { exact: true })).toBeVisible();
  await select(page, "每页", "25");
  await expect(page.getByText("1–25 / 27", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("checkbox", { name: "选择当前页任务" }),
  ).toBeChecked();
  await page
    .getByRole("textbox", { name: "搜索任务", exact: true })
    .fill("source-26");
  await expect(page.getByText("1–1 / 1", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("button", { name: "取消选择", exact: true }),
  ).toHaveCount(0);
  await page
    .getByRole("checkbox", { name: "选择任务 2126", exact: true })
    .check();
  await page.getByRole("button", { name: "启动", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("任务：#2126");
  await expect(dialog).not.toContainText("#2125");
  await dialog.getByRole("button", { name: "确认启动", exact: true }).click();
  await expect
    .poll(
      () =>
        requests.filter((request) => request.path === "/api/master/start")
          .length,
    )
    .toBe(1);
  expect(
    requests.find((request) => request.path === "/api/master/start")?.body,
  ).toEqual({ taskIds: [2126], expectedGenerations: { "2126": 1 } });
});

for (const signature of [
  { label: "使用服务端默认值", value: null },
  { label: "Signature V4", value: false },
  { label: "Signature V2", value: true },
]) {
  test(`storage ${signature.label} combines every addressing mode and validation choice without changing credentials`, async ({
    page,
  }) => {
    test.setTimeout(90_000);
    await mockApi(page);
    let connection: Record<string, unknown> = {
      ...storageConfigs[0],
      useV2SignerType: true,
      pathStyleEnable: false,
    };
    const writes: { method: string; body: any }[] = [];
    await page.route("**/api/master/s3-config**", async (route) => {
      const request = route.request();
      if (request.method() === "GET") {
        await route.fulfill({ json: { success: true, configs: [connection] } });
      } else {
        const body = request.postDataJSON();
        writes.push({ method: request.method(), body });
        connection = body;
        await route.fulfill({ json: { success: true } });
      }
    });
    await page.goto("/storage");
    for (const addressing of [
      { label: "使用服务端默认值", value: null },
      { label: "Path-style", value: true },
      { label: "Virtual-hosted", value: false },
    ]) {
      for (const validate of [true, false]) {
        await page
          .getByRole("button", { name: "编辑连接 production", exact: true })
          .click();
        const dialog = page.getByRole("dialog", { name: "编辑存储连接" });
        await expect(dialog.getByLabel("连接名称")).toBeDisabled();
        await select(page, "签名版本", signature.label);
        await select(page, "寻址方式", addressing.label);
        await dialog
          .getByRole("switch", { name: "保存时验证访问权限" })
          .setChecked(validate);
        const previous = writes.length;
        await dialog
          .getByRole("button", { name: "保存连接", exact: true })
          .click();
        await expect(dialog).toHaveCount(0);
        expect(writes).toHaveLength(previous + 1);
        expect(writes.at(-1)).toEqual({
          method: "PUT",
          body: {
            ...storageConfigs[0],
            accessKey: "",
            secretKey: "",
            useV2SignerType: signature.value,
            pathStyleEnable: addressing.value,
            validate,
          },
        });
      }
    }
  });
}

test("storage duplicate names are rejected and Escape discards unsaved credentials", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.goto("/storage");
  await page.getByRole("button", { name: "添加连接", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "添加存储连接" });
  await dialog.getByLabel("连接名称").fill(" production ");
  await dialog.getByLabel("Endpoint").fill("https://new.example.test");
  await dialog.getByLabel("Access Key").fill("QA_FAKE_AK");
  await dialog.getByLabel("Secret Key").fill("QA_FAKE_SK");
  await dialog.getByRole("button", { name: "保存连接", exact: true }).click();
  await expect(
    dialog.getByText("连接名称已存在，请更换名称或编辑现有连接。"),
  ).toBeVisible();
  await expect(
    dialog.getByRole("button", { name: "显示访问凭据" }),
  ).toHaveCount(0);
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await page.getByRole("button", { name: "添加连接", exact: true }).click();
  await expect(dialog.getByLabel("连接名称")).toHaveValue("");
  await expect(dialog.getByLabel("Access Key")).toHaveValue("");
  await expect(dialog.getByLabel("Secret Key")).toHaveValue("");
  await expect(dialog.getByLabel("Secret Key")).toHaveAttribute(
    "type",
    "password",
  );
  expect(requests.filter((request) => request.method !== "GET")).toHaveLength(
    0,
  );
});

test("reversed and equal modification dates cannot create tasks, while a one-sided bound submits exactly", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await fillOss(page);
  await page.getByRole("button", { name: /迁移选项/ }).click();
  await page.getByLabel("修改时间晚于（可选）").fill("2026-09-23T10:00");
  for (const before of ["2026-09-22T10:00", "2026-09-23T10:00"]) {
    await page.getByLabel("修改时间早于（可选）").fill(before);
    await page
      .getByRole("button", { name: "创建迁移任务", exact: true })
      .click();
    await expect(
      page.getByRole("alert").filter({ hasText: "结束时间必须晚于开始时间。" }),
    ).toBeVisible();
    expect(
      requests.filter((request) => request.path === "/api/master/add-buckets"),
    ).toHaveLength(0);
  }
  await page.getByLabel("修改时间早于（可选）").fill("");
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
  expect(body.dateAfter).toBe("2026-09-23T10:00:00");
  expect(body).not.toHaveProperty("dateBefore");
});

test("relative paths and duplicate worker bindings fail before precheck or task creation", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.goto("/create");
  await page.getByRole("button", { name: /对象存储 → 本地/ }).click();
  await fillSource(page);
  await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
  await page.getByRole("option", { name: /10\.0\.1\.12/ }).click();
  await page.getByLabel("目标目录绝对路径").fill("relative/path");
  await page.getByRole("button", { name: "运行预检", exact: true }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "请输入执行节点上的绝对路径。" }),
  ).toBeVisible();
  await page.getByLabel("目标目录绝对路径").fill("/mnt/datasets/one");
  await page.getByRole("button", { name: /添加共享存储节点/ }).click();
  await page
    .getByRole("combobox", { name: "执行节点", exact: true })
    .nth(1)
    .click();
  await expect(
    page.getByRole("option", { name: /10\.0\.1\.12/ }),
  ).toHaveAttribute("aria-disabled", "true");
  await page.keyboard.press("Escape");
  await page.evaluate(() => {
    const draft = JSON.parse(
      sessionStorage.getItem("oss-transfer-create-draft")!,
    );
    draft.form.bindings[1].executionNodeIp =
      draft.form.bindings[0].executionNodeIp;
    sessionStorage.setItem("oss-transfer-create-draft", JSON.stringify(draft));
  });
  await page.reload();
  await page.getByLabel("目标目录绝对路径").nth(1).fill("/mnt/datasets/two");
  await page.getByRole("button", { name: "运行预检", exact: true }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "同一个执行节点只能绑定一次。" }),
  ).toBeVisible();
  expect(requests.filter((request) => request.method === "POST")).toHaveLength(
    0,
  );
});

test("empty source ranges are blocked and changing to whole bucket removes the old prefix", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await fillOss(page);
  await select(page, "迁移范围", "按对象前缀筛选");
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
  expect(
    await page
      .getByLabel("源对象前缀")
      .evaluate((el) => (el as HTMLInputElement).validity.valueMissing),
  ).toBe(true);
  expect(
    requests.filter((request) => request.path === "/api/master/add-buckets"),
  ).toHaveLength(0);
  await page.getByLabel("源对象前缀").fill("old-prefix/");
  await select(page, "迁移范围", "整个存储桶");
  await expect(page.getByLabel("源对象前缀")).toHaveCount(0);
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
  await expect
    .poll(
      () =>
        requests.filter((request) => request.path === "/api/master/add-buckets")
          .length,
    )
    .toBe(1);
  expect(
    requests.find((request) => request.path === "/api/master/add-buckets")!
      .body[0],
  ).not.toHaveProperty("prefix");
});

test("help and account menus close with Escape and restore focus without submitting account changes", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.goto("/tasks");
  const help = page.getByRole("button", { name: "使用帮助", exact: true });
  await help.click();
  const dialog = page.getByRole("dialog", { name: "操作说明" });
  await expect(dialog).toContainText("任务创建后自动执行");
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(help).toBeFocused();
  const account = page.getByRole("button", { name: "账号设置", exact: true });
  await account.click();
  await expect(page.getByRole("menuitem", { name: "修改密码" })).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("menu")).toHaveCount(0);
  await expect(account).toBeFocused();
  await expect(page).toHaveURL("/tasks");
  expect(requests.filter((request) => request.method !== "GET")).toHaveLength(
    0,
  );
});

test("unknown task details have no actions and refresh the same requested ID without losing other query parameters", async ({
  page,
}) => {
  await mockApi(page);
  let available = false;
  let reads = 0;
  await page.route("**/api/master/status", (route) => {
    reads++;
    return route.fulfill({
      json: {
        tasks: available ? [...tasks, { ...tasks[1], taskIndex: 9999 }] : tasks,
      },
    });
  });
  await page.goto("/tasks?taskId=9999&source=matrix");
  const panel = page
    .locator(".MuiDrawer-paper")
    .filter({ has: page.getByRole("heading", { name: "迁移任务 #9999" }) });
  await expect(panel).toContainText("当前列表中未找到此任务，请刷新任务状态。");
  await expect(
    panel.getByRole("button", {
      name: /启动任务|停止任务|继续传输|删除任务|导出任务报告/,
    }),
  ).toHaveCount(0);
  const before = reads;
  available = true;
  await panel.getByRole("button", { name: "刷新", exact: true }).click();
  await expect.poll(() => reads).toBeGreaterThan(before);
  await expect(
    panel.getByRole("button", { name: "启动任务", exact: true }),
  ).toBeVisible();
  await expect(panel).not.toContainText("当前列表中未找到此任务");
  await expect
    .poll(() =>
      panel.evaluate((element) => element.contains(document.activeElement)),
    )
    .toBe(true);
  await page.keyboard.press("Escape");
  await expect(panel).toHaveCount(0);
  await expect(page).toHaveURL("/tasks?source=matrix");
});
