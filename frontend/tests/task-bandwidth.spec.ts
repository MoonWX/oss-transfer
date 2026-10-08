import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

async function select(page: Page, label: string, option: string) {
  await page.getByRole("combobox", { name: label }).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}
async function configure(page: Page, direction = "OSS_TO_OSS") {
  await page.goto("/create");
  if (direction !== "OSS_TO_OSS")
    await page
      .getByRole("button", {
        name:
          direction === "OSS_TO_LOCAL" ? /对象存储 → 本地/ : /本地 → 对象存储/,
      })
      .click();
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
    await page.getByRole("combobox", { name: "执行节点 IP" }).fill("10.0.1.12");
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
const status = (limitMbps: number | null = null) => ({
  limitMbps,
  applied: true,
  sharedTaskCount: 1,
  canEdit: true,
  nodes: [] as { address: string; applied: boolean; message?: string }[],
});
const detailPath = "**/api/master/tasks/1048/bandwidth-limit";
const limitField = (page: Page) =>
  page.getByRole("spinbutton", { name: "任务带宽上限", exact: true });

for (const direction of ["OSS_TO_OSS", "OSS_TO_LOCAL", "LOCAL_TO_OSS"]) {
  test(`${direction} submits one task-wide Mbps value unchanged through precheck and creation`, async ({
    page,
  }) => {
    const requests = await mockApi(page);
    await configure(page, direction);
    await limitField(page).fill("12.5");
    await expect(page.getByText(/· 上限 12.5 Mbps/)).toBeVisible();
    if (direction !== "OSS_TO_OSS") {
      await page.getByRole("button", { name: "运行预检", exact: true }).click();
      await expect(page.getByText("所有节点预检通过").first()).toBeVisible();
      expect(
        requests.find((r) => r.path === "/api/master/local-transfers/validate")!
          .body[0].taskBandwidthLimitMbps,
      ).toBe(12.5);
    }
    await page
      .getByRole("button", { name: "创建迁移任务", exact: true })
      .click();
    const path =
      direction === "OSS_TO_OSS"
        ? "/api/master/add-buckets"
        : "/api/master/local-transfers";
    await expect
      .poll(
        () =>
          requests.filter((r) => r.path === path && r.method === "POST").length,
      )
      .toBe(1);
    expect(
      requests.find((r) => r.path === path && r.method === "POST")!.body[0]
        .taskBandwidthLimitMbps,
    ).toBe(12.5);
  });
}

test("optional task bandwidth validates bounds and keeps an empty setting out of legacy payloads", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await configure(page);
  for (const value of ["-1", "0", "0.09", "1000000.1", "1e100"]) {
    await limitField(page).fill(value);
    await expect(limitField(page)).toHaveAttribute("aria-invalid", "true");
    await page
      .getByRole("button", { name: "创建迁移任务", exact: true })
      .click();
    expect(
      requests.filter((r) => r.path === "/api/master/add-buckets"),
    ).toHaveLength(0);
  }
  for (const value of ["0.1", "1000000", ""]) {
    await limitField(page).fill(value);
    await expect(limitField(page)).toHaveAttribute("aria-invalid", "false");
    await page
      .getByRole("button", { name: "创建迁移任务", exact: true })
      .click();
    await expect(
      page.getByRole("button", { name: "继续配置任务", exact: true }),
    ).toBeVisible();
    const body = requests
      .filter((r) => r.path === "/api/master/add-buckets")
      .at(-1)!.body[0];
    if (value) expect(body.taskBandwidthLimitMbps).toBe(Number(value));
    else expect(body).not.toHaveProperty("taskBandwidthLimitMbps");
    await page
      .getByRole("button", { name: "继续配置任务", exact: true })
      .click();
  }
});

test("bandwidth draft survives navigation direction changes and reload while reset clears it", async ({
  page,
}) => {
  await mockApi(page);
  await configure(page);
  await limitField(page).fill("75.5");
  await page.getByRole("button", { name: /对象存储 → 本地/ }).click();
  await expect(limitField(page)).toHaveValue("75.5");
  await page
    .getByRole("navigation", { name: "主导航" })
    .getByRole("link", { name: "存储连接", exact: true })
    .click();
  await page
    .getByRole("navigation", { name: "主导航" })
    .getByRole("link", { name: "创建迁移", exact: true })
    .click();
  await page.reload();
  await page.getByRole("button", { name: /迁移选项/ }).click();
  await expect(limitField(page)).toHaveValue("75.5");
  await page.getByRole("button", { name: "重置草稿", exact: true }).click();
  await expect(limitField(page)).toHaveValue("");
});

test("batch JSON preserves each optional limit, rejects invalid types, and old five-column CSV stays unchanged", async ({
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
  for (const invalid of ["10", 0, -1, 1e100]) {
    await page
      .getByLabel("任务数据")
      .fill(JSON.stringify([{ ...base, taskBandwidthLimitMbps: invalid }]));
    await page.getByRole("button", { name: "导入任务", exact: true }).click();
    await expect(page.getByText(/第 1 项：带宽上限需为/)).toBeVisible();
    expect(
      requests.filter((r) => r.path === "/api/master/add-buckets"),
    ).toHaveLength(0);
  }
  const rows = [
    { ...base, taskBandwidthLimitMbps: 50 },
    { ...base, taskBandwidthLimitMbps: null },
  ];
  await page.getByLabel("任务数据").fill(JSON.stringify(rows));
  await page.getByRole("button", { name: "导入任务", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "继续配置任务", exact: true }),
  ).toBeVisible();
  expect(
    requests.filter((r) => r.path === "/api/master/add-buckets").at(-1)!.body,
  ).toEqual(rows);
  await page.getByRole("button", { name: "继续配置任务", exact: true }).click();
  await select(page, "数据格式", "逗号分隔文本");
  await page
    .getByLabel("任务数据")
    .fill("production/media-archive,backup/media-backup,,false,balanced");
  await page.getByRole("button", { name: "导入任务", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "继续配置任务", exact: true }),
  ).toBeVisible();
  expect(
    requests.filter((r) => r.path === "/api/master/add-buckets").at(-1)!.body,
  ).toEqual([{ ...base, overwrite: false, distributionMode: "balanced" }]);
  await page.getByRole("button", { name: "继续配置任务", exact: true }).click();
  await select(page, "导入类型", "本地迁移请求");
  const localRow = {
    direction: "OSS_TO_LOCAL",
    sourceEndpoint: {
      type: "OSS",
      s3Alias: "production",
      bucket: "media-archive",
    },
    destinationEndpoint: {
      type: "LOCAL",
      nodeBindings: [
        { executionNodeIp: "10.0.1.12", localPath: "/mnt/datasets/qa" },
      ],
    },
    taskBandwidthLimitMbps: 25,
  };
  await page.getByLabel("任务数据").fill(JSON.stringify([localRow]));
  await page.getByRole("button", { name: "运行预检", exact: true }).click();
  await expect(page.getByText("所有节点预检通过").first()).toBeVisible();
  expect(
    requests.find((r) => r.path === "/api/master/local-transfers/validate")!
      .body,
  ).toEqual([localRow]);
  await page.getByRole("button", { name: "导入任务", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "继续配置任务", exact: true }),
  ).toBeVisible();
  expect(
    requests.find((r) => r.path === "/api/master/local-transfers")!.body,
  ).toEqual([localRow]);
  await page.getByRole("button", { name: "继续配置任务", exact: true }).click();
  await select(page, "导入类型", "OSS 子任务恢复 / 导入");
  await page
    .getByLabel("任务数据")
    .fill(JSON.stringify([{ ...base, taskBandwidthLimitMbps: 15 }]));
  await page.getByRole("button", { name: "导入任务", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "继续配置任务", exact: true }),
  ).toBeVisible();
  expect(requests.find((r) => r.path === "/api/master/import")!.body).toEqual([
    { ...base, taskBandwidthLimitMbps: 15 },
  ]);
});

test("running task bandwidth adjusts down and up, cancel does not write, and clearing sends explicit null", async ({
  page,
}) => {
  await mockApi(page);
  let current = status(100);
  current.sharedTaskCount = 3;
  const writes: unknown[] = [];
  await page.route(detailPath, async (route) => {
    if (route.request().method() === "PUT") {
      writes.push(route.request().postDataJSON());
      current = {
        ...current,
        limitMbps: route.request().postDataJSON().limitMbps,
      };
    }
    await route.fulfill({ json: { success: true, data: current } });
  });
  await page.goto("/tasks?taskId=1048");
  await expect(
    page.getByText("带宽上限 100 Mbps", { exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "调整", exact: true }).click();
  await expect(
    page.getByText("3 个子任务共享此上限。", { exact: true }),
  ).toBeVisible();
  await limitField(page).fill("1");
  await page.getByRole("button", { name: "取消", exact: true }).click();
  expect(writes).toEqual([]);
  for (const value of ["50", "200", ""]) {
    await page.getByRole("button", { name: "调整", exact: true }).click();
    await limitField(page).fill(value);
    await page.getByRole("button", { name: "应用", exact: true }).click();
    await expect(
      page.getByRole("dialog", { name: "任务 #1048 限速" }),
    ).toHaveCount(0);
    expect(writes.at(-1)).toEqual({ limitMbps: value ? Number(value) : null });
  }
  await expect(
    page.getByRole("button", { name: "限速", exact: true }),
  ).toBeVisible();
  await expect(page.getByText(/带宽上限 .* Mbps/)).toHaveCount(0);
});

test("partial application shows failed nodes without success and retry applies the same saved value", async ({
  page,
}) => {
  await mockApi(page);
  const writes: unknown[] = [];
  await page.route(detailPath, async (route) => {
    const result = status();
    if (route.request().method() === "PUT") {
      writes.push(route.request().postDataJSON());
      result.limitMbps = route.request().postDataJSON().limitMbps;
      result.applied = writes.length > 1;
      result.nodes = [
        { address: "10.0.1.12", applied: true },
        {
          address: "10.0.1.13",
          applied: result.applied,
          message: result.applied ? undefined : "节点不可达",
        },
      ];
    }
    await route.fulfill({ json: { success: true, data: result } });
  });
  await page.goto("/tasks?taskId=1048");
  await page.getByRole("button", { name: "限速", exact: true }).click();
  await limitField(page).fill("25");
  await page.getByRole("button", { name: "应用", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "任务 #1048 限速" });
  await expect(dialog.getByRole("alert")).toContainText("部分节点未生效");
  await expect(dialog).toContainText("10.0.1.13：节点不可达");
  await expect(dialog).not.toContainText("10.0.1.12");
  await expect(
    page.getByText("任务带宽上限已更新。", { exact: true }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: "重试应用", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(writes).toEqual([{ limitMbps: 25 }, { limitMbps: 25 }]);
  await expect(
    page.getByText("任务带宽上限已更新。", { exact: true }),
  ).toBeVisible();
});

test("delayed and failed settings never become an unlimited label, retry can recover into a noneditable task", async ({
  page,
}) => {
  await mockApi(page);
  let release!: () => void;
  const delayed = new Promise<void>((resolve) => {
    release = resolve;
  });
  let fail = true;
  await page.route(detailPath, async (route) => {
    if (fail) {
      await delayed;
      await route.fulfill({
        status: 503,
        json: { success: false, message: "节点信息暂不可用" },
      });
    } else
      await route.fulfill({
        json: { success: true, data: { ...status(80), canEdit: false } },
      });
  });
  await page.goto("/tasks?taskId=1048");
  await expect(page.getByText("读取限速设置…", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("button", { name: "限速", exact: true }),
  ).toHaveCount(0);
  release();
  await expect(
    page.getByText(/无法读取任务限速：节点信息暂不可用/),
  ).toBeVisible();
  fail = false;
  await page.getByRole("button", { name: "重试读取任务限速" }).click();
  await expect(
    page.getByText("带宽上限 80 Mbps", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "调整", exact: true }),
  ).toBeDisabled();
});

test("bandwidth controls fit desktop light dark and mobile views and failed save retains editable input", async ({
  page,
}, testInfo) => {
  await mockApi(page);
  let failSave = true;
  await page.route(detailPath, async (route) => {
    if (route.request().method() === "PUT" && failSave)
      await route.fulfill({
        status: 503,
        json: { success: false, message: "设置未保存，请重试" },
      });
    else
      await route.fulfill({
        json: { success: true, data: { ...status(100), sharedTaskCount: 4 } },
      });
  });
  await page.goto("/tasks?taskId=1048");
  await page.getByRole("button", { name: "调整", exact: true }).click();
  await expect(page.getByRole("dialog")).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("task-bandwidth-desktop-light.png"),
    animations: "disabled",
  });
  await page.getByRole("button", { name: "取消", exact: true }).click();
  await page.getByRole("button", { name: "关闭详情" }).click();
  await page.getByRole("button", { name: "切换主题" }).click();
  await page.goto("/tasks?taskId=1048");
  await page.getByRole("button", { name: "调整", exact: true }).click();
  await expect(page.locator("html")).toHaveCSS("color-scheme", "dark");
  await page.screenshot({
    path: testInfo.outputPath("task-bandwidth-desktop-dark.png"),
    animations: "disabled",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await limitField(page).fill("123.5");
  await page.getByRole("button", { name: "应用", exact: true }).click();
  await expect(page.getByRole("dialog").getByRole("alert")).toContainText(
    "设置未保存，请重试",
  );
  await expect(limitField(page)).toHaveValue("123.5");
  await page.screenshot({
    path: testInfo.outputPath("task-bandwidth-mobile-dark.png"),
    animations: "disabled",
  });
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(390);
  const dialog = (await page.getByRole("dialog").boundingBox())!;
  expect(dialog.x).toBeGreaterThanOrEqual(0);
  expect(dialog.x + dialog.width).toBeLessThanOrEqual(390);
  failSave = false;
  await page.getByRole("button", { name: "取消", exact: true }).click();
});

test("slow background refresh does not block editing or overwrite a newer saved limit", async ({
  page,
}) => {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  let holdReads = false;
  let waiting = false;
  let released = false;
  let current = 100;
  let release!: () => void;
  const delayed = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route(detailPath, async (route) => {
    if (route.request().method() === "PUT") {
      current = route.request().postDataJSON().limitMbps;
      holdReads = false;
    }
    const captured = current;
    if (holdReads) {
      waiting = true;
      await delayed;
      released = true;
    }
    await route.fulfill({ json: { success: true, data: status(captured) } });
  });
  await page.goto("/tasks?taskId=1048");
  await expect(
    page.getByText("带宽上限 100 Mbps", { exact: true }),
  ).toBeVisible();
  holdReads = true;
  await expect.poll(() => waiting).toBe(true);
  await expect(
    page.getByRole("button", { name: "调整", exact: true }),
  ).toBeEnabled();
  await page.getByRole("button", { name: "调整", exact: true }).click();
  await limitField(page).fill("50");
  await page.getByRole("button", { name: "应用", exact: true }).click();
  await expect(
    page.getByText("带宽上限 50 Mbps", { exact: true }),
  ).toBeVisible();
  release();
  await expect.poll(() => released).toBe(true);
  await expect(
    page.getByText("带宽上限 100 Mbps", { exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByText("带宽上限 50 Mbps", { exact: true }),
  ).toBeVisible();
});

test("switching related task IDs with a delayed read cannot reuse or save another task's limit", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/task-creations/bandwidth-pair", (route) =>
    route.fulfill({
      json: {
        success: true,
        creation: {
          id: "bandwidth-pair",
          status: "COMPLETED",
          taskIds: [1048, 1047],
        },
      },
    }),
  );
  let release!: () => void;
  const delayed = new Promise<void>((resolve) => {
    release = resolve;
  });
  const writes: { path: string; value: unknown }[] = [];
  await page.route("**/api/master/tasks/*/bandwidth-limit", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (route.request().method() === "PUT") {
      writes.push({ path, value: route.request().postDataJSON() });
      await route.fulfill({
        json: {
          success: true,
          data: status(route.request().postDataJSON().limitMbps),
        },
      });
      return;
    }
    if (path.includes("/1047/")) await delayed;
    await route.fulfill({
      json: { success: true, data: status(path.includes("/1047/") ? 25 : 100) },
    });
  });
  await page.goto("/tasks?creationId=bandwidth-pair&taskId=1048");
  await expect(
    page.getByText("带宽上限 100 Mbps", { exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "#1047", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1047", exact: true }),
  ).toBeVisible();
  await expect(page.getByText("读取限速设置…", { exact: true })).toBeVisible();
  await expect(
    page.getByText("带宽上限 100 Mbps", { exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "调整", exact: true }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: "#1048", exact: true }).click();
  await expect(
    page.getByText("带宽上限 100 Mbps", { exact: true }),
  ).toBeVisible();
  release();
  await expect(page.getByText("带宽上限 25 Mbps", { exact: true })).toHaveCount(
    0,
  );
  await page.getByRole("button", { name: "#1047", exact: true }).click();
  await expect(
    page.getByText("带宽上限 25 Mbps", { exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "调整", exact: true }).click();
  await expect(limitField(page)).toHaveValue("25");
  await limitField(page).fill("40");
  await page.getByRole("button", { name: "应用", exact: true }).click();
  await expect(
    page.getByRole("dialog", { name: "任务 #1047 限速" }),
  ).toHaveCount(0);
  expect(writes).toEqual([
    {
      path: "/api/master/tasks/1047/bandwidth-limit",
      value: { limitMbps: 40 },
    },
  ]);
});

test("a lost PUT response rechecks actual state and confirms success only when all nodes applied the requested value", async ({
  page,
}) => {
  await mockApi(page);
  let current = 100;
  let reads = 0;
  let applied = true;
  const writes: number[] = [];
  await page.route(detailPath, async (route) => {
    if (route.request().method() === "PUT") {
      current = route.request().postDataJSON().limitMbps;
      writes.push(current);
      await route.abort("failed");
      return;
    }
    reads++;
    await route.fulfill({
      json: {
        success: true,
        data: {
          ...status(current),
          applied,
          nodes: applied
            ? []
            : [{ address: "10.0.1.13", applied: false, message: "节点不可达" }],
        },
      },
    });
  });
  await page.goto("/tasks?taskId=1048");
  await expect(
    page.getByText("带宽上限 100 Mbps", { exact: true }),
  ).toBeVisible();
  const before = reads;
  await page.getByRole("button", { name: "调整", exact: true }).click();
  await limitField(page).fill("60");
  await page.getByRole("button", { name: "应用", exact: true }).click();
  await expect(
    page.getByRole("dialog", { name: "任务 #1048 限速" }),
  ).toHaveCount(0);
  expect(reads).toBeGreaterThan(before);
  await expect(
    page.getByText("已核对：任务带宽上限已生效。", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("带宽上限 60 Mbps", { exact: true }),
  ).toBeVisible();
  await page
    .getByRole("alert", { includeHidden: true })
    .getByRole("button", { name: "Close", includeHidden: true })
    .click();
  applied = false;
  await page.getByRole("button", { name: "调整", exact: true }).click();
  await limitField(page).fill("30");
  await page.getByRole("button", { name: "应用", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "任务 #1048 限速" });
  await expect(dialog).toContainText("部分节点未生效");
  await expect(dialog).toContainText("应用结果未确认");
  await expect(limitField(page)).toHaveValue("30");
  await expect(
    page.getByText("已核对：任务带宽上限已生效。", { exact: true }),
  ).toHaveCount(0);
  expect(writes).toEqual([60, 30]);
});

for (const taskState of ["COMPLETED", "RUNNING", "STOPPED"]) {
  test(`${taskState} task bandwidth edit availability follows task lifecycle`, async ({
    page,
  }) => {
    await mockApi(page);
    await page.route("**/api/master/status", (route) =>
      route.fulfill({
        json: { totalTasks: 1, tasks: [{ ...tasks[0], status: taskState }] },
      }),
    );
    let writes = 0;
    await page.route(detailPath, (route) => {
      if (route.request().method() === "PUT") writes++;
      return route.fulfill({ json: { success: true, data: status(100) } });
    });
    await page.goto("/tasks?taskId=1048");
    const edit = page.getByRole("button", { name: "调整", exact: true });
    if (taskState === "COMPLETED") {
      await expect(edit).toBeDisabled();
      await expect(
        page.getByText("带宽上限 100 Mbps", { exact: true }),
      ).toBeVisible();
      expect(writes).toBe(0);
    } else {
      await expect(edit).toBeEnabled();
      await edit.click();
      await limitField(page).fill("50");
      await page.getByRole("button", { name: "应用", exact: true }).click();
      await expect.poll(() => writes).toBe(1);
    }
  });
}

test("an open bandwidth editor cannot submit after task completion", async ({
  page,
}) => {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  let taskState = "RUNNING";
  let writes = 0;
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: { totalTasks: 1, tasks: [{ ...tasks[0], status: taskState }] },
    }),
  );
  await page.route(detailPath, (route) => {
    if (route.request().method() === "PUT") writes++;
    return route.fulfill({ json: { success: true, data: status(100) } });
  });
  await page.goto("/tasks?taskId=1048");
  await page.getByRole("button", { name: "调整", exact: true }).click();
  await limitField(page).fill("50");
  taskState = "COMPLETED";
  const dialog = page.getByRole("dialog", { name: "任务 #1048 限速" });
  await expect(
    dialog.getByRole("button", { name: "应用", exact: true }),
  ).toBeDisabled();
  await expect(limitField(page)).toBeDisabled();
  await dialog
    .locator("form")
    .evaluate((form) =>
      form.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      ),
    );
  expect(writes).toBe(0);
});
