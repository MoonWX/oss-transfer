import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

async function fixture(page: Page, container = false) {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-locale", "zh"),
  );
  const state = {
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
    if (path.endsWith("/master")) state.master.update = update;
    return route.fulfill({
      status: 202,
      json: { success: true, data: update },
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
