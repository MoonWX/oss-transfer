import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";
import {
  compareVersions,
  isPrerelease,
  newerVersion,
  validVersion,
} from "../src/lib/semver";

async function fixture(page: Page, masterVersion = "4.1.28", locale = "zh-CN") {
  await mockApi(page);
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (message) => {
    if (message.type() === "error" && /TypeError/.test(message.text()))
      errors.push(message.text());
  });
  await page.addInitScript(
    (value) => localStorage.setItem("oss-transfer-language", value),
    locale,
  );
  const state = {
    queue: { phase: "IDLE", version: "", items: [] as unknown[] },
    master: {
      info: {
        role: "master",
        version: masterVersion,
        platform: "linux/amd64",
        supported: true,
      },
      update: { phase: "IDLE" } as Record<string, string>,
    },
    agents: [
      {
        address: "agent-new",
        displayName: "New",
        info: {
          role: "agent",
          version: "4.1.28",
          supported: true,
          prereleaseUpdates: true,
        },
        update: { phase: "IDLE" },
      },
      {
        address: "agent-old",
        displayName: "Old",
        info: { role: "agent", version: "4.1.28", supported: true },
        update: { phase: "IDLE" },
      },
    ],
    latest: { version: "4.1.28" },
    available: false,
    checkedAt: new Date().toISOString(),
  };
  const requests: { path: string; body: unknown }[] = [];
  await page.route("**/api/master/software-updates**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (route.request().method() === "GET")
      return route.fulfill({ json: { success: true, data: state } });
    const body = route.request().postDataJSON();
    requests.push({ path, body });
    const update = { phase: "QUEUED", version: body.version };
    if (path.endsWith("/master")) {
      state.master.update = update;
      return route.fulfill({
        status: 202,
        json: { success: true, data: update },
      });
    }
    state.queue = {
      phase: "RUNNING",
      version: body.version,
      items: [{ address: path.split("/").pop(), phase: "QUEUED" }],
    };
    return route.fulfill({
      status: 202,
      json: { success: true, data: state.queue },
    });
  });
  return { state, requests, errors };
}

test("console version comparison follows SemVer precedence", () => {
  const chain = [
    "4.1.28",
    "4.1.29-beta.1",
    "4.1.29-beta.2",
    "4.1.29-beta.10",
    "4.1.29-rc.1",
    "4.1.29",
  ];
  for (let i = 0; i < chain.length; i++)
    for (let j = 0; j < chain.length; j++)
      expect(compareVersions(chain[i], chain[j])).toBe(Math.sign(i - j));
  for (const invalid of [
    "4.1",
    "04.1.29",
    "4.1.29-beta.01",
    "4.1.29+build",
    "4.1.0-dev",
    "4.1.29-",
  ])
    expect(validVersion(invalid), invalid).toBe(false);
  expect(isPrerelease("4.1.29-rc.1")).toBe(true);
  expect(isPrerelease("4.1.29")).toBe(false);
  expect(newerVersion("4.1.29", "4.1.0-dev")).toBe(false);
});

test("specific-version entry is hidden without the prerelease channel parameter", async ({
  page,
}) => {
  const f = await fixture(page);
  await page.goto("/tools?tab=updates");
  await expect(page.getByText("已是最新版本", { exact: true })).toBeVisible();
  await expect(page.getByRole("region", { name: "指定版本" })).toHaveCount(0);
  await expect(page.getByRole("textbox", { name: "版本号" })).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "安装指定版本", exact: true }),
  ).toHaveCount(0);
  // Other channel values do not reveal it, and the node page never shows it.
  await page.goto("/tools?tab=updates&channel=beta");
  await expect(page.getByText("已是最新版本", { exact: true })).toBeVisible();
  await expect(page.getByRole("textbox", { name: "版本号" })).toHaveCount(0);
  await page.goto("/nodes?channel=prerelease");
  await page.getByRole("tab", { name: "版本更新", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "更新全部节点", exact: true }),
  ).toBeVisible();
  await expect(page.getByRole("textbox", { name: "版本号" })).toHaveCount(0);
  expect(f.requests).toEqual([]);
  expect(f.errors).toEqual([]);
});

test("prerelease channel installs an exact version only after a warning confirmation", async ({
  page,
}) => {
  const f = await fixture(page);
  await page.goto("/tools?tab=updates&channel=prerelease");
  const entry = page.getByRole("region", { name: "指定版本" });
  await expect(entry).toBeVisible();
  const install = entry.getByRole("button", {
    name: "安装指定版本",
    exact: true,
  });
  await expect(install).toBeDisabled();
  const input = entry.getByRole("textbox", { name: "版本号" });
  for (const invalid of [
    "4.1.27-beta.1",
    "4.1.29-beta.01",
    "latest",
    "4.1.30",
  ]) {
    await input.fill(invalid);
    await expect(install).toBeDisabled();
    await expect(
      entry.getByText("请输入比当前更新的预发布版本号，如 4.1.29-beta.1", {
        exact: true,
      }),
    ).toBeVisible();
  }
  await input.fill("v4.1.29-beta.1");
  await expect(install).toBeEnabled();
  await install.click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("安装实验版本 4.1.29-beta.1？");
  await expect(dialog).toContainText("不会出现在正常更新中");
  // Cancelling sends nothing.
  await dialog.getByRole("button", { name: "取消", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(f.requests).toEqual([]);
  await install.click();
  await dialog
    .getByRole("button", { name: "确认安装实验版本", exact: true })
    .click();
  await expect(dialog).toBeHidden();
  expect(f.requests).toEqual([
    {
      path: "/api/master/software-updates/master",
      body: { version: "4.1.29-beta.1", allowPrerelease: true },
    },
  ]);
  await expect(page.getByText("更新已排队", { exact: true })).toBeVisible();
  expect(f.errors).toEqual([]);
});

test("prerelease channel entry is translated", async ({ page }) => {
  await fixture(page, "4.1.28", "en-US");
  await page.goto("/tools?tab=updates&channel=prerelease");
  const entry = page.getByRole("region", { name: "Specific version" });
  await entry.getByRole("textbox", { name: "Version" }).fill("4.1.29-rc.1");
  await entry
    .getByRole("button", { name: "Install this version", exact: true })
    .click();
  await expect(page.getByRole("dialog")).toContainText(
    "Install experimental build 4.1.29-rc.1?",
  );
  await expect(page.getByRole("dialog")).toContainText(
    "never offered by normal updates",
  );
});

test("agents follow a prerelease Master only when they accept the explicit flag", async ({
  page,
}) => {
  const f = await fixture(page, "4.1.29-beta.1");
  await page.goto("/tools?tab=updates");
  const rows = page
    .getByRole("table", { name: "节点版本", exact: true })
    .getByRole("row");
  await expect(
    rows.nth(1).getByRole("button", { name: "更新", exact: true }),
  ).toBeEnabled();
  await expect(
    rows.nth(2).getByRole("button", { name: "更新", exact: true }),
  ).toBeDisabled();
  await rows.nth(1).getByRole("button", { name: "更新", exact: true }).click();
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "确认更新", exact: true })
    .click();
  // The Master's own version is the explicit choice; no extra flag is sent.
  expect(f.requests).toEqual([
    {
      path: "/api/master/software-updates/agents/agent-new",
      body: { version: "4.1.29-beta.1" },
    },
  ]);
  await expect(page.getByText("更新已排队", { exact: true })).toBeVisible();
  expect(f.errors).toEqual([]);
});
