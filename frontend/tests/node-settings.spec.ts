import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

async function fixture(page: Page) {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  let reachable = true;
  let children = 32;
  let objects = 4;
  const roots = ["/mnt/data", "C:\\Archive"];
  const writes: Record<string, unknown>[] = [];
  await page.route("**/api/master/nodes**", async (route) => {
    if (route.request().method() === "PUT") {
      const body = route.request().postDataJSON();
      writes.push(body);
      if (body.maxConcurrentChildren !== undefined)
        children = body.maxConcurrentChildren;
      if (body.objectsPerChild !== undefined) objects = body.objectsPerChild;
    }
    await route.fulfill({
      json: {
        success: true,
        data: {
          nodes: [
            {
              address: "storage-node",
              label: "Storage",
              agent: true,
              online: reachable,
            },
          ],
          port: 1685,
        },
      },
    });
  });
  await page.route("**/api/master/worker-status", (route) =>
    route.fulfill({
      json: {
        success: true,
        workers: {
          "storage-node": reachable
            ? {
                success: true,
                executionStatus: {
                  activeScans: 1,
                  maxConcurrentScans: 2,
                  activeChildren: 4,
                  maxConcurrentChildren: children,
                  objectsPerChild: objects,
                  unfinishedChildren: 80,
                  maxUnfinishedChildren: 1024,
                },
                localStorage: { advertisedLocalRoots: roots },
              }
            : { success: false, message: "Agent disconnected" },
        },
      },
    }),
  );
  return {
    writes,
    offline: () => {
      reachable = false;
    },
  };
}

test("node workload remains visible and renaming preserves legacy directory settings", async ({
  page,
}) => {
  const state = await fixture(page);
  await page.goto("/nodes");
  const region = page.getByRole("region", {
    name: "执行节点 Storage",
    exact: true,
  });
  await expect(region.getByText("80 / 1,024", { exact: true })).toBeVisible();
  for (const label of ["扫描并发", "子任务并发", "未完成子任务"]) {
    await expect(
      region.getByRole("progressbar", { name: label, exact: true }),
    ).toBeVisible();
  }
  await expect(region.getByText("队列占用", { exact: true })).toHaveCount(0);
  await expect(region.getByRole("progressbar")).toHaveCount(3);
  await region.getByRole("button", { name: "编辑节点", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "编辑节点", exact: true });
  await expect(
    dialog.getByRole("textbox", { name: "节点标识", exact: true }),
  ).toHaveCount(0);
  await expect(dialog.getByLabel("本地根目录", { exact: true })).toHaveCount(0);
  await dialog.getByLabel("节点名称", { exact: true }).fill("Archive");
  await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(state.writes[0]).toEqual({
    address: "storage-node",
    label: "Archive",
  });
  await expect(region.getByText("本地根目录", { exact: true })).toHaveCount(0);
  await expect(region.getByText("/mnt/data", { exact: true })).toHaveCount(0);
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(region.getByText("80 / 1,024", { exact: true })).toBeVisible();
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(390);
});

test("renaming and unavailable-node editing do not send an empty root replacement", async ({
  page,
}) => {
  const state = await fixture(page);
  await page.goto("/nodes");
  const edit = page.getByRole("button", { name: "编辑节点", exact: true });
  await edit.click();
  let dialog = page.getByRole("dialog", { name: "编辑节点", exact: true });
  await dialog.getByLabel("节点名称", { exact: true }).fill("Renamed");
  await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(state.writes[0]).toEqual({
    address: "storage-node",
    label: "Renamed",
  });
  state.offline();
  await expect(
    page.getByText("Agent disconnected", { exact: true }),
  ).toBeVisible();
  await edit.click();
  dialog = page.getByRole("dialog", { name: "编辑节点", exact: true });
  await expect(dialog.getByLabel("本地根目录", { exact: true })).toHaveCount(0);
  await dialog.getByLabel("节点名称", { exact: true }).fill("Offline name");
  await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(state.writes[1]).toEqual({
    address: "storage-node",
    label: "Offline name",
  });
});

test("node concurrency applies through the settings dialog and is retained when reopened", async ({
  page,
}) => {
  const state = await fixture(page);
  await page.goto("/nodes");
  await page.getByRole("button", { name: "编辑节点", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "编辑节点", exact: true });
  await expect(
    dialog.getByRole("spinbutton", { name: "子任务并发", exact: true }),
  ).toHaveValue("32");
  await expect(
    dialog.getByRole("spinbutton", {
      name: "每个子任务的对象并发",
      exact: true,
    }),
  ).toHaveValue("4");
  await dialog
    .getByRole("spinbutton", { name: "子任务并发", exact: true })
    .fill("12");
  await dialog
    .getByRole("spinbutton", { name: "每个子任务的对象并发", exact: true })
    .fill("6");
  await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(state.writes).toEqual([
    {
      address: "storage-node",
      label: "Storage",
      maxConcurrentChildren: 12,
      objectsPerChild: 6,
    },
  ]);
  await expect(page.getByText("4 / 12", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "编辑节点", exact: true }).click();
  await expect(
    dialog.getByRole("spinbutton", { name: "子任务并发", exact: true }),
  ).toHaveValue("12");
  await expect(
    dialog.getByRole("spinbutton", {
      name: "每个子任务的对象并发",
      exact: true,
    }),
  ).toHaveValue("6");
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(
    dialog.getByRole("spinbutton", {
      name: "每个子任务的对象并发",
      exact: true,
    }),
  ).toBeVisible();
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(390);
});

test("invalid combined concurrency stays in the editor without submitting", async ({
  page,
}) => {
  const state = await fixture(page);
  await page.goto("/nodes");
  await page.getByRole("button", { name: "编辑节点", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "编辑节点", exact: true });
  await dialog
    .getByRole("spinbutton", { name: "子任务并发", exact: true })
    .fill("256");
  await dialog
    .getByRole("spinbutton", { name: "每个子任务的对象并发", exact: true })
    .fill("64");
  await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
  await expect(dialog.getByRole("alert")).toContainText(
    "两者乘积不能超过 1024",
  );
  expect(state.writes).toEqual([]);
});

const scanHelp =
  "同时运行的扫描数，1–16，默认 2。每个对象存储扫描同一时间有一个 LIST 请求在途，所有扫描共用节点的 LIST 上限；每个清单扫描最多同时发出 16 个 HEAD，受节点的 HEAD 上限约束；每个本地目录扫描最多同时查询 32 个条目，源端为 NFS 时调高会挤占传输所需的连接。";
const scanLowering =
  "调低后，正在运行的扫描不会中断；运行中的扫描数少于新上限后，才会开始新的扫描。";
const scanFixed = "此节点的 Agent 版本较旧，扫描并发固定为 2；升级后才能修改。";
const scanOverLimit =
  "正在运行的扫描多于新上限；它们陆续结束后，按新上限开始新的扫描。";

/** One Agent whose scan concurrency the console may edit when protocol is set. */
async function scanFixture(
  page: Page,
  options: { protocol?: number; active?: number; scans?: number } = {},
) {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  let scans = options.scans ?? 2;
  const writes: Record<string, unknown>[] = [];
  await page.route("**/api/master/nodes**", async (route) => {
    if (route.request().method() === "PUT") {
      const body = route.request().postDataJSON();
      writes.push(body);
      if (body.maxConcurrentScans !== undefined)
        scans = body.maxConcurrentScans;
    }
    await route.fulfill({
      json: {
        success: true,
        data: {
          nodes: [
            {
              address: "storage-node",
              label: "Storage",
              agent: true,
              online: true,
            },
          ],
          port: 1685,
        },
      },
    });
  });
  await page.route("**/api/master/worker-status", (route) =>
    route.fulfill({
      json: {
        success: true,
        workers: {
          "storage-node": {
            success: true,
            executionStatus: {
              activeScans: options.active ?? 1,
              maxConcurrentScans: scans,
              activeChildren: 4,
              maxConcurrentChildren: 32,
              objectsPerChild: 4,
              unfinishedChildren: 80,
              maxUnfinishedChildren: 1024,
              ...(options.protocol === undefined
                ? {}
                : { scanConcurrencyProtocol: options.protocol }),
            },
          },
        },
      },
    }),
  );
  return { writes };
}

test("scan concurrency is saved on its own and explains lowering", async ({
  page,
}) => {
  const state = await scanFixture(page, { protocol: 1 });
  await page.goto("/nodes");
  const region = page.getByRole("region", {
    name: "执行节点 Storage",
    exact: true,
  });
  await expect(region.getByText("1 / 2", { exact: true })).toBeVisible();
  await expect(page.getByText(scanOverLimit, { exact: true })).toHaveCount(0);
  await region.getByRole("button", { name: "编辑节点", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "编辑节点", exact: true });
  const field = dialog.getByRole("spinbutton", {
    name: "扫描并发",
    exact: true,
  });
  await expect(field).toHaveValue("2");
  await expect(field).toBeEditable();
  await expect(dialog.getByText(scanHelp, { exact: true })).toBeVisible();
  await expect(dialog.getByText(scanFixed, { exact: true })).toHaveCount(0);
  await field.fill("6");
  await expect(dialog.getByText(scanLowering, { exact: true })).toHaveCount(0);
  await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(state.writes).toEqual([
    { address: "storage-node", label: "Storage", maxConcurrentScans: 6 },
  ]);
  await expect(region.getByText("1 / 6", { exact: true })).toBeVisible();
  await region.getByRole("button", { name: "编辑节点", exact: true }).click();
  await expect(field).toHaveValue("6");
  await field.fill("1");
  await expect(dialog.getByText(scanLowering, { exact: true })).toBeVisible();
  await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(state.writes[1]).toEqual({
    address: "storage-node",
    label: "Storage",
    maxConcurrentScans: 1,
  });
  // A rename leaves the scan concurrency out of the change.
  await region.getByRole("button", { name: "编辑节点", exact: true }).click();
  await dialog.getByLabel("节点名称", { exact: true }).fill("Archive");
  await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(state.writes[2]).toEqual({
    address: "storage-node",
    label: "Archive",
  });
});

test("invalid scan concurrency stays in the editor without submitting", async ({
  page,
}) => {
  const state = await scanFixture(page, { protocol: 1 });
  await page.goto("/nodes");
  await page.getByRole("button", { name: "编辑节点", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "编辑节点", exact: true });
  const field = dialog.getByRole("spinbutton", {
    name: "扫描并发",
    exact: true,
  });
  for (const value of ["0", "17", "2.5", ""]) {
    await field.fill(value);
    await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
    await expect(dialog.getByRole("alert")).toHaveText(
      "扫描并发必须是 1–16 的整数。",
    );
  }
  expect(state.writes).toEqual([]);
  // The subtask product limit does not count the scans.
  await field.fill("16");
  await dialog
    .getByRole("spinbutton", { name: "子任务并发", exact: true })
    .fill("256");
  await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(state.writes).toEqual([
    {
      address: "storage-node",
      label: "Storage",
      maxConcurrentChildren: 256,
      objectsPerChild: 4,
      maxConcurrentScans: 16,
    },
  ]);
});

test("an older Agent shows its fixed scan concurrency read-only", async ({
  page,
}) => {
  const state = await scanFixture(page);
  await page.goto("/nodes");
  await page.getByRole("button", { name: "编辑节点", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "编辑节点", exact: true });
  const field = dialog.getByRole("spinbutton", {
    name: "扫描并发",
    exact: true,
  });
  await expect(field).toHaveValue("2");
  await expect(field).not.toBeEditable();
  await expect(dialog.getByText(scanFixed, { exact: true })).toBeVisible();
  await expect(dialog.getByText(scanHelp, { exact: true })).toHaveCount(0);
  await dialog.getByLabel("节点名称", { exact: true }).fill("Archive");
  await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(state.writes).toEqual([{ address: "storage-node", label: "Archive" }]);
});

test("a node running more scans than its lowered limit says what happens", async ({
  page,
}) => {
  await scanFixture(page, { protocol: 1, active: 3, scans: 1 });
  await page.goto("/nodes");
  const region = page.getByRole("region", {
    name: "执行节点 Storage",
    exact: true,
  });
  await expect(region.getByText("3 / 1", { exact: true })).toBeVisible();
  await expect(region.getByText(scanOverLimit, { exact: true })).toBeVisible();
});

test.describe("scan concurrency in English", () => {
  test.use({ locale: "en-US" });
  test("the field, its help and its checks are translated", async ({
    page,
  }) => {
    const state = await scanFixture(page, { protocol: 1, scans: 4 });
    await page.goto("/nodes");
    await page.getByRole("button", { name: "Edit node", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Edit node", exact: true });
    const field = dialog.getByRole("spinbutton", {
      name: "Scan concurrency",
      exact: true,
    });
    await expect(field).toHaveValue("4");
    await expect(
      dialog.getByText(
        "Scans that can run at once, 1–16, default 2. Each object storage scan keeps one LIST request in flight, and all scans share the node's LIST limit; each file-list scan keeps up to 16 HEAD requests in flight within the node's HEAD limit; each local directory scan keeps up to 32 entry lookups in flight, so on an NFS source a higher value takes connections that transfers need.",
        { exact: true },
      ),
    ).toBeVisible();
    await field.fill("3");
    await expect(
      dialog.getByText(
        "Lowering it does not interrupt running scans; new scans start only once fewer scans than the new limit are running.",
        { exact: true },
      ),
    ).toBeVisible();
    await field.fill("20");
    await dialog
      .getByRole("button", { name: "Save node", exact: true })
      .click();
    await expect(dialog.getByRole("alert")).toHaveText(
      "Scan concurrency must be a whole number from 1 to 16.",
    );
    expect(state.writes).toEqual([]);
  });
});

test.describe("scan concurrency in Japanese", () => {
  test.use({ locale: "ja-JP" });
  test("an older Agent and an over-limit node are explained", async ({
    page,
  }) => {
    await scanFixture(page, { active: 3, scans: 2 });
    await page.goto("/nodes");
    await expect(
      page.getByText(
        "新しい上限を超える数のスキャンが実行中です。これらが終わるにつれ、新しい上限に従ってスキャンを開始します。",
        { exact: true },
      ),
    ).toBeVisible();
    await page
      .getByRole("button", { name: "ノードを編集", exact: true })
      .click();
    const dialog = page.getByRole("dialog", {
      name: "ノードを編集",
      exact: true,
    });
    await expect(
      dialog.getByRole("spinbutton", { name: "スキャン並列数", exact: true }),
    ).not.toBeEditable();
    await expect(
      dialog.getByText(
        "このノードの Agent はバージョンが古く、スキャン並列数は 2 に固定されています。変更するにはアップグレードしてください。",
        { exact: true },
      ),
    ).toBeVisible();
  });
});
