import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

async function fixture(page: Page, container = false) {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-locale", "zh"),
  );
  const state = {
    queue: {
      phase: "IDLE",
      version: "",
      items: [] as {
        address: string;
        phase: string;
        reason?: string;
        error?: string;
      }[],
    },
    master: {
      info: {
        role: "master",
        version: "4.1.14",
        platform: "linux/amd64",
        supported: !container,
        reason: container ? "CONTAINER" : "",
      },
      update: { phase: "IDLE" },
    },
    agents: [
      {
        address: "agent-a",
        displayName: "Archive",
        displayAddress: "10.0.1.10",
        info: { role: "agent", version: "4.1.13", supported: true },
        update: { phase: "IDLE" },
      },
      {
        address: "agent-b",
        displayName: "Offline",
        info: null,
        unavailable: true,
      },
    ],
    latest: { version: "4.1.15" },
    available: true,
    checkedAt: new Date().toISOString(),
  };
  const requests: { path: string; body: unknown }[] = [];
  let offline = false;
  await page.route("**/api/master/software-updates**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (route.request().method() === "GET") {
      if (offline) return route.abort();
      return route.fulfill({ json: { success: true, data: state } });
    }
    requests.push({ path, body: route.request().postDataJSON() });
    const update = {
      phase: "QUEUED",
      version: route.request().postDataJSON().version,
    };
    const agentUpdate = path.includes("/agents");
    if (path.endsWith("/master")) state.master.update = update;
    if (agentUpdate)
      state.queue = {
        phase: "RUNNING",
        version: update.version,
        items: state.agents
          .filter(
            (node) =>
              path.endsWith("/agents") || path.endsWith("/" + node.address),
          )
          .map((node) => ({ address: node.address, phase: "QUEUED" })),
      };
    return route.fulfill({
      status: 202,
      json: { success: true, data: agentUpdate ? state.queue : update },
    });
  });
  return {
    state,
    requests,
    disconnect: () => {
      offline = true;
    },
  };
}

test("master update confirms its version, prevents repeats and keeps status during restart", async ({
  page,
}) => {
  const f = await fixture(page);
  await page.goto("/tools?tab=updates");
  await expect(page.getByText("新版本 4.1.15", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "更新 Master", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("4.1.15");
  await dialog.getByRole("button", { name: "确认更新", exact: true }).click();
  await expect(dialog).toBeHidden();
  await expect(
    page.getByRole("button", { name: "更新 Master", exact: true }),
  ).toBeDisabled();
  expect(f.requests).toEqual([
    {
      path: "/api/master/software-updates/master",
      body: { version: "4.1.15" },
    },
  ]);
  f.disconnect();
  await expect(
    page.getByText("正在重新连接主节点…", { exact: true }),
  ).toBeVisible({ timeout: 10000 });
  await expect(page.getByText("4.1.14", { exact: true })).toBeVisible();
  await expect(page.getByText("更新已排队", { exact: true })).toBeVisible();
});

test("agent update targets the installed Master and unavailable agents cannot update", async ({
  page,
}) => {
  const f = await fixture(page);
  await page.goto("/tools?tab=updates");
  const rows = page
    .getByRole("table", { name: "节点版本", exact: true })
    .getByRole("row");
  await expect(
    rows.nth(2).getByRole("button", { name: "更新", exact: true }),
  ).toBeDisabled();
  await rows.nth(1).getByRole("button", { name: "更新", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("4.1.14");
  await expect(dialog).not.toContainText("4.1.15");
  await dialog.getByRole("button", { name: "确认更新", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(f.requests).toEqual([
    {
      path: "/api/master/software-updates/agents/agent-a",
      body: { version: "4.1.14" },
    },
  ]);
});

test("container installation shows Compose commands and never offers in-container replacement", async ({
  page,
}) => {
  const f = await fixture(page, true);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/tools?tab=updates");
  await expect(
    page.getByText("Docker 部署请更新镜像后重新创建容器。", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText(/docker compose pull/)).toBeVisible();
  await expect(
    page.getByRole("button", { name: "更新 Master", exact: true }),
  ).toBeDisabled();
  expect(f.requests).toEqual([]);
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(390);
});

test("node page queues updates once and restores progress after reopening", async ({
  page,
}) => {
  const f = await fixture(page);
  await page.goto("/nodes");
  await page.getByRole("tab", { name: "版本更新", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "检测新版本", exact: true }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: "更新全部节点", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("共 1 个可升级节点");
  await dialog.getByRole("button", { name: "确认更新", exact: true }).click();
  await expect(dialog).toBeHidden();
  await expect(
    page.getByRole("button", { name: "更新全部节点", exact: true }),
  ).toBeDisabled();
  expect(f.requests).toEqual([
    {
      path: "/api/master/software-updates/agents",
      body: { version: "4.1.14" },
    },
  ]);
  f.state.queue.items[0].phase = "UPDATING";
  Object.assign(f.state.agents[0], { unavailable: true });
  await page.reload();
  await page.getByRole("tab", { name: "版本更新", exact: true }).click();
  await expect(
    page.getByText("正在更新，等待节点重新连接…", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "更新全部节点", exact: true }),
  ).toBeDisabled();
  await expect(
    page.getByText("节点正在逐台更新，关闭页面不影响进度。", { exact: true }),
  ).toBeVisible();
  f.state.queue.phase = "FAILED";
  f.state.queue.items[0] = {
    address: "agent-a",
    phase: "FAILED",
    error: "Not enough free space",
  };
  f.state.queue.items[1] = {
    address: "agent-b",
    phase: "SKIPPED",
    reason: "PREVIOUS_FAILED",
  };
  Object.assign(f.state.agents[0], { unavailable: false });
  await expect(
    page.getByText("节点更新已停止，请处理失败原因后重试。", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("Not enough free space", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("前一节点更新失败，尚未更新", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "更新全部节点", exact: true }),
  ).toBeEnabled();
});

test("legacy Agent instructions differ from an offline Agent", async ({
  page,
}) => {
  const f = await fixture(page);
  Object.assign(f.state.agents[0], {
    info: { role: "agent", version: "", supported: false, reason: "LEGACY" },
    upgradeReason: "LEGACY",
  });
  await page.goto("/tools?tab=updates");
  await expect(
    page.getByText("此版本需先用安装脚本升级一次，之后可在这里更新。", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(
    page.getByText("节点离线，重新连接后可更新。", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "更新全部节点", exact: true }),
  ).toBeDisabled();
});
