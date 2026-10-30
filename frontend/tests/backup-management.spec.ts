import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

const GiB = 1024 ** 3;
async function fixture(page: Page) {
  await mockApi(page);
  const software = {
    master: { info: { version: "4.1.19", supported: true } },
    agents: [],
    available: false,
    checkedAt: new Date().toISOString(),
  };
  const backups = {
    policy: { keep: 0, maxAgeDays: 0 },
    master: {
      supported: true,
      count: 4,
      bytes: 10 * GiB,
      reclaimableCount: 2,
      reclaimableBytes: 6 * GiB,
    },
    agents: [
      {
        address: "agent-a",
        displayName: "Archive",
        displayAddress: "10.0.1.10",
        supported: true,
        reason: "",
        count: 2,
        bytes: 4 * GiB,
        reclaimableCount: 0,
        reclaimableBytes: 0,
      },
      {
        address: "agent-b",
        displayName: "Offline",
        displayAddress: "10.0.1.11",
        supported: false,
        reason: "OFFLINE",
        count: 0,
        bytes: 0,
        reclaimableCount: 0,
        reclaimableBytes: 0,
      },
    ],
  };
  const requests: { path: string; method: string; body: unknown }[] = [];
  let softwareReads = 0;
  let failure = "";
  let release: (() => void) | undefined;
  let gate: Promise<void> | undefined;
  await page.route("**/api/master/software-updates**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (!path.includes("/backups")) {
      softwareReads++;
      return route.fulfill({ json: { success: true, data: software } });
    }
    requests.push({
      path,
      method: request.method(),
      body: request.postDataJSON(),
    });
    if (request.method() === "GET")
      return route.fulfill({ json: { success: true, data: backups } });
    if (gate) await gate;
    if (failure)
      return route.fulfill({
        status: 409,
        json: { success: false, message: failure },
      });
    const summary = path.endsWith("/master/prune")
      ? backups.master
      : backups.agents.find((node) => path.includes(`/${node.address}/`))!;
    const data = {
      removedCount: summary.reclaimableCount,
      freedBytes: summary.reclaimableBytes,
    };
    summary.count -= data.removedCount;
    summary.bytes -= data.freedBytes;
    summary.reclaimableCount = 0;
    summary.reclaimableBytes = 0;
    return route.fulfill({ json: { success: true, data } });
  });
  return {
    backups,
    requests,
    softwareReads: () => softwareReads,
    fail: (message: string) => {
      failure = message;
    },
    hold: () => {
      gate = new Promise<void>((resolve) => {
        release = resolve;
      });
    },
    release: () => {
      release?.();
      gate = undefined;
    },
  };
}

test("backups load on demand and cleanup can remove every eligible backup without keeping the newest", async ({
  page,
}) => {
  const f = await fixture(page);
  Object.assign(f.backups.master, {
    reclaimableCount: 4,
    reclaimableBytes: 10 * GiB,
  });
  await page.goto("/tools?tab=updates");
  const section = page.getByRole("button", { name: "升级备份", exact: true });
  await expect(section).toBeVisible();
  expect(f.requests).toEqual([]);
  await section.click();
  await expect(
    page.getByText("升级成功后删除本次备份；失败或缺少完成标记的备份会保留。", {
      exact: true,
    }),
  ).toBeVisible();
  const table = page.getByRole("table", { name: "升级备份", exact: true });
  const master = table.getByRole("row").filter({ hasText: "Master" });
  await expect(master).toContainText("10 GiB");
  const reads = f.softwareReads();
  await expect.poll(f.softwareReads).toBeGreaterThan(reads);
  expect(f.requests.filter((request) => request.method === "GET")).toHaveLength(
    1,
  );
  await section.click();
  await section.click();
  expect(f.requests.filter((request) => request.method === "GET")).toHaveLength(
    1,
  );
  const archive = table
    .getByRole("row")
    .filter({ hasText: "Archive (10.0.1.10)" });
  await expect(
    archive.getByRole("button", { name: "清理备份" }),
  ).toBeDisabled();
  await master.getByRole("button", { name: "清理备份" }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("删除 4 份备份，释放约 10 GiB");
  f.hold();
  await dialog
    .getByRole("button", { name: "确认清理" })
    .evaluate((button: HTMLButtonElement) => {
      button.click();
      button.click();
    });
  await expect(dialog.getByRole("button", { name: "正在清理" })).toBeDisabled();
  await expect(dialog.getByRole("button", { name: "取消" })).toBeDisabled();
  await expect
    .poll(
      () => f.requests.filter((request) => request.method === "POST").length,
    )
    .toBe(1);
  f.release();
  await expect(dialog).toBeHidden();
  await expect(master.getByRole("cell").nth(1)).toHaveText("0");
  await expect(master.getByRole("cell").nth(2)).toHaveText("0 B");
  await expect(master.getByRole("button", { name: "清理备份" })).toBeDisabled();
  await expect(
    page.getByText("已清理 4 份备份，释放 10 GiB。", { exact: true }),
  ).toBeVisible();
  expect(f.requests.filter((request) => request.method === "POST")).toEqual([
    {
      path: "/api/master/software-updates/backups/master/prune",
      method: "POST",
      body: {},
    },
  ]);
});

test("offline backup data stays unknown; failed cleanup preserves totals and can be retried", async ({
  page,
}) => {
  const f = await fixture(page);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/nodes");
  await page.getByRole("tab", { name: "版本更新", exact: true }).click();
  await page.getByRole("button", { name: "升级备份", exact: true }).click();
  const table = page.getByRole("table", { name: "升级备份", exact: true });
  await expect(table).not.toContainText("Master");
  const offline = table
    .getByRole("row")
    .filter({ hasText: "Offline (10.0.1.11)" });
  await expect(offline).toContainText("节点离线，无法读取备份。");
  await expect(offline.getByRole("button")).toBeDisabled();
  Object.assign(f.backups.agents[1], {
    supported: true,
    reason: "",
    count: 3,
    bytes: 8 * GiB,
    reclaimableCount: 1,
    reclaimableBytes: 2 * GiB,
  });
  await page.getByRole("button", { name: "刷新备份", exact: true }).click();
  await expect(offline).toContainText("8 GiB");
  f.fail("节点正在更新或清理备份，请稍后刷新。");
  await offline.getByRole("button", { name: "清理备份" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("button", { name: "确认清理" }).click();
  await expect(dialog.getByRole("alert")).toContainText("节点正在更新");
  await expect(
    page
      .getByRole("table", {
        name: "升级备份",
        exact: true,
        includeHidden: true,
      })
      .getByRole("row", { includeHidden: true })
      .filter({ hasText: "Offline (10.0.1.11)" }),
  ).toContainText("8 GiB");
  await expect
    .poll(() => f.requests.filter((request) => request.method === "GET").length)
    .toBe(3);
  f.fail("");
  await dialog.getByRole("button", { name: "确认清理" }).click();
  await expect(dialog).toBeHidden();
  await expect(offline).toContainText("6 GiB");
  await expect(offline.getByRole("button")).toBeDisabled();
  expect(
    f.requests
      .filter((request) => request.method === "POST")
      .map((request) => request.path),
  ).toEqual([
    "/api/master/software-updates/backups/agents/agent-b/prune",
    "/api/master/software-updates/backups/agents/agent-b/prune",
  ]);
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(390);
});

test("a partly completed cleanup refreshes remaining backups without resubmitting deletion", async ({
  page,
}) => {
  const f = await fixture(page);
  await page.goto("/tools?tab=updates");
  await page.getByRole("button", { name: "升级备份", exact: true }).click();
  const master = page
    .getByRole("table", { name: "升级备份", exact: true })
    .getByRole("row")
    .filter({ hasText: "Master" });
  await master.getByRole("button", { name: "清理备份" }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("删除 2 份备份，释放约 6 GiB");
  f.hold();
  f.fail("部分备份清理失败。");
  await dialog.getByRole("button", { name: "确认清理" }).click();
  await expect
    .poll(
      () => f.requests.filter((request) => request.method === "POST").length,
    )
    .toBe(1);
  Object.assign(f.backups.master, {
    count: 3,
    bytes: 8 * GiB,
    reclaimableCount: 1,
    reclaimableBytes: 4 * GiB,
  });
  f.release();
  await expect(dialog.getByRole("alert")).toHaveText("部分备份清理失败。");
  await expect(dialog).toContainText("删除 1 份备份，释放约 4 GiB");
  await expect(dialog.getByRole("button", { name: "确认清理" })).toBeEnabled();
  expect(
    f.requests.filter((request) => request.method === "POST"),
  ).toHaveLength(1);
  await dialog.getByRole("button", { name: "取消" }).click();
  await expect(master).toContainText("8 GiB");
  await expect(master.getByRole("cell").nth(1)).toHaveText("3");
  expect(f.requests.filter((request) => request.method === "GET")).toHaveLength(
    2,
  );
});

test("backup management switches English and Japanese without another disk scan", async ({
  page,
}) => {
  const f = await fixture(page);
  await page.goto("/tools?tab=updates");
  await page.getByRole("button", { name: "升级备份", exact: true }).click();
  await expect(
    page.getByRole("table", { name: "升级备份", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "语言", exact: true }).click();
  await page.getByRole("menuitem", { name: "English", exact: true }).click();
  await expect(
    page.getByRole("table", { name: "Upgrade backups", exact: true }),
  ).toContainText("Disk usage");
  await page.getByRole("button", { name: "Language", exact: true }).click();
  await page.getByRole("menuitem", { name: "日本語", exact: true }).click();
  await expect(
    page.getByRole("table", { name: "更新時のバックアップ", exact: true }),
  ).toContainText("使用容量");
  expect(f.requests).toHaveLength(1);
});
