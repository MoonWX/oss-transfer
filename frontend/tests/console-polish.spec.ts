import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";
import {
  historyItem,
  pageOf,
  progress,
  routeComparisons,
  rows,
} from "./object-comparison-fixtures";
import { buildLabel } from "../src/lib/buildAge";
import { containerAgentCommands } from "../src/lib/containerAgent";
import { passwordRuleError, passwordRuleText } from "../src/lib/passwordRule";
import { displayVersion } from "../src/lib/semver";
import { t, translate } from "../src/lib/i18n";
import "./catalogs";

test("versions are shown as v<version> with the commit as detail", () => {
  expect(buildLabel("4.2.0", "33b320a")).toBe("v4.2.0 (33b320a)");
  expect(buildLabel("v4.2.0", "33b320a")).toBe("v4.2.0 (33b320a)");
  expect(buildLabel("", "33b320a")).toBe("33b320a");
  expect(displayVersion("4.1.41")).toBe("v4.1.41");
  expect(displayVersion("v4.2.0-rc.1")).toBe("v4.2.0-rc.1");
  expect(displayVersion("")).toBe("");
});

test("the console checks the same password rule as the Master", () => {
  expect(passwordRuleError("abc", "initial", "admin")).toBe(
    t("密码至少需要 8 个字符"),
  );
  // Characters, not bytes: seven CJK characters are too short.
  expect(passwordRuleError("密码密码密码密", "", "admin")).toBe(
    t("密码至少需要 8 个字符"),
  );
  expect(passwordRuleError(" Operator ", "x", "operator")).toBe(
    t("密码不能与用户名相同"),
  );
  expect(passwordRuleError("same-password", "same-password", "admin")).toBe(
    t("新密码不能与当前密码相同"),
  );
  expect(passwordRuleError("long enough", "initial", "admin")).toBe("");
  // Trivial passwords are rejected like the Master does.
  for (const weak of [
    "12345678",
    "Password",
    " QWERTY123 ",
    "aaaaaaaa",
    "98765432",
    "abcdefgh",
    "HGFEDCBA",
  ])
    expect(passwordRuleError(weak, "initial", "admin")).toBe(
      t("密码过于常见或容易猜到，请换一个"),
    );
  for (const fine of ["12345679", "6789:;<=", "aaaaaaab"])
    expect(passwordRuleError(fine, "initial", "admin")).toBe("");
  const rule =
    "至少 8 个字符，不能是常见密码、同一字符重复或连续的数字或字母（如 12345678、abcdefgh），也不能与用户名或当前密码相同。";
  expect(passwordRuleText()).toBe(t(rule));
  for (const locale of ["en-US", "ja-JP"] as const) {
    expect(translate(locale, rule)).not.toBe(rule);
    expect(translate(locale, "密码过于常见或容易猜到，请换一个")).not.toBe(
      "密码过于常见或容易猜到，请换一个",
    );
  }
});

test("the change-password dialog states the rule and rejects the current password locally", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.goto("/");
  await page.getByRole("button", { name: "账号设置" }).click();
  await page.getByRole("menuitem", { name: "修改密码" }).click();
  const dialog = page.getByRole("dialog");
  await expect(
    dialog.getByText(
      "至少 8 个字符，不能是常见密码、同一字符重复或连续的数字或字母（如 12345678、abcdefgh），也不能与用户名或当前密码相同。",
    ),
  ).toBeVisible();
  await dialog.getByLabel(/^当前密码/).fill("current-password");
  await dialog.getByLabel(/^新密码/).fill("current-password");
  await dialog.getByLabel(/^确认新密码/).fill("current-password");
  await dialog.getByRole("button", { name: "保存密码" }).click();
  await expect(page.getByText("新密码不能与当前密码相同")).toBeVisible();
  expect(
    requests.filter((r) => r.path === "/api/auth/change-password"),
  ).toEqual([]);
});

test("the sidebar footer shows the release version before the commit", async ({
  page,
}) => {
  await mockApi(page);
  await page.goto("/");
  await expect(page.locator("footer code")).toHaveText(
    /^v\d+\.\d+\.\d+\S* \(\S+\)$/,
  );
});

async function emptyCluster(page: Page) {
  await mockApi(page, { empty: true });
  await page.route("**/api/master/nodes", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: { nodes: [], port: 1685, masterWebUrl: "" },
      },
    }),
  );
  await page.route("**/api/master/s3-config", (route) =>
    route.fulfill({ json: { success: true, configs: [] } }),
  );
}

test("the first-run checklist starts with adding a node and shows each step's state", async ({
  page,
}) => {
  await emptyCluster(page);
  await page.goto("/");
  const checklist = page.getByRole("region", { name: "开始使用" });
  const steps = checklist.getByRole("listitem");
  await expect(steps).toHaveCount(4);
  await expect(steps.nth(0)).toContainText("添加执行节点");
  await expect(steps.nth(0)).toContainText("尚未添加节点");
  await expect(steps.nth(1)).toContainText("添加存储连接");
  await expect(steps.nth(1)).toContainText("尚未添加连接");
  await expect(
    page.getByText("先添加执行节点和存储连接，再创建迁移任务。"),
  ).toBeVisible();
  await checklist.getByRole("link", { name: "添加节点" }).click();
  await expect(page).toHaveURL(/\/nodes\?add=1$/);
  await expect(page.getByRole("dialog")).toBeVisible();
});

test("the help dialog lists the node step first with the current counts", async ({
  page,
}) => {
  await mockApi(page);
  await page.goto("/");
  await page.getByRole("button", { name: "使用帮助" }).click();
  const steps = page.getByRole("dialog").getByRole("listitem");
  await expect(steps.nth(0)).toContainText("添加执行节点");
  await expect(steps.nth(0)).toContainText("已添加 2 个节点");
  await expect(steps.nth(1)).toContainText("已添加 2 个连接");
  await steps.nth(1).getByRole("link", { name: "管理存储连接" }).click();
  await expect(page).toHaveURL(/\/storage$/);
  await expect(page.getByRole("dialog")).toHaveCount(0);
});

test("the help dialog's add-node link opens the add dialog on the nodes page too", async ({
  page,
}) => {
  await mockApi(page);
  await page.goto("/nodes");
  await expect(page.getByRole("heading", { name: "执行节点" })).toBeVisible();
  for (let round = 0; round < 2; round++) {
    await page.getByRole("button", { name: "使用帮助" }).click();
    await page
      .getByRole("dialog")
      .getByRole("listitem")
      .nth(0)
      .getByRole("link", { name: "添加节点" })
      .click();
    await expect(page).toHaveURL(/\/nodes\?add=1$/);
    const dialog = page.getByRole("dialog", { name: "添加节点" });
    await expect(dialog).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dialog).toBeHidden();
    await expect(page).toHaveURL(/\/nodes$/);
  }
});

const multiNode = {
  ...tasks[3],
  taskIndex: 1050,
  status: "COMPLETED",
  prefix: "logs/",
  keyMapping: { mode: "ADD_PREFIX", targetPrefix: "archive" },
  executionNodes: [
    { address: "10.0.1.12", displayName: "ot-eval-native-master" },
    { address: "10.0.1.13", displayName: "agent2" },
    { address: "10.0.1.14", displayName: "agent3" },
  ],
};

async function taskList(page: Page) {
  await mockApi(page);
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: 5, tasks: [multiNode, ...tasks] } }),
  );
  await page.goto("/tasks");
}

test("task rows open their details and list one node per line", async ({
  page,
}) => {
  await taskList(page);
  const row = page.getByRole("row").filter({ hasText: "#1050" });
  const nodes = row.getByLabel(/10\.0\.1\.12.*10\.0\.1\.13.*10\.0\.1\.14/);
  await expect(nodes.locator("div")).toHaveText([
    "master (10.0.1.12)",
    "agent (10.0.1.13)",
    "另 1 个节点",
  ]);
  await row.getByText("production/application-logs").click();
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1050" }),
  ).toBeVisible();
});

test("a task's details open object comparison for its source and target", async ({
  page,
}) => {
  await taskList(page);
  await routeComparisons(page, {
    list: () => ({ items: [], complete: true, unavailableNodes: [] }),
  });
  await page.getByRole("button", { name: "查看任务 1050" }).click();
  await page.getByRole("link", { name: "比对源和目标" }).click();
  await expect(page).toHaveURL(/\/tools\?/);
  await expect(page.getByRole("textbox", { name: "源存储桶" })).toHaveValue(
    "production/application-logs",
  );
  await expect(page.getByRole("textbox", { name: "目标存储桶" })).toHaveValue(
    "backup/logs-archive",
  );
  await expect(page.getByRole("textbox", { name: "源前缀" })).toHaveValue(
    "logs/",
  );
  await expect(page.getByRole("textbox", { name: "目标前缀" })).toHaveValue(
    "archive/logs/",
  );
});

test("a single-object content comparison shows its read rate and resumes", async ({
  page,
}) => {
  await mockApi(page);
  const single = {
    ...progress,
    id: "single",
    scope: "object",
    mode: "content",
    status: "RUNNING",
    compared: 0,
    matched: 0,
    different: 0,
    etagMismatch: 0,
    missing: 0,
    extra: 0,
    rows: 0,
    bytesRead: 600 * 1024 * 1024,
    elapsedMillis: 60000,
    resumes: 1,
  };
  await routeComparisons(page, {
    list: () => ({
      items: [historyItem(single)],
      complete: true,
      unavailableNodes: [],
    }),
    page: (_, query) => pageOf(single, [], query),
  });
  await page.goto("/tools?comparison=single");
  const result = page.getByRole("region", { name: "比对结果" });
  await expect(
    result.getByText("已用 1 分 0 秒，已读取 600 MiB（10 MiB/秒）"),
  ).toBeVisible();
  await expect(result.getByText(/个\/秒/)).toHaveCount(0);
  await expect(
    result.getByText("已从检查点继续 1 次（如节点重启后）"),
  ).toBeVisible();
});

test("the export button says it exports every difference, whatever the filter", async ({
  page,
}) => {
  await mockApi(page);
  await routeComparisons(page, {
    list: () => ({
      items: [historyItem()],
      complete: true,
      unavailableNodes: [],
    }),
    page: (_, query) => pageOf(progress, rows, query),
  });
  await page.goto("/tools?comparison=comparison-1");
  const result = page.getByRole("region", { name: "比对结果" });
  await expect(
    result.getByRole("button", { name: "导出全部差异（ZIP）" }),
  ).toBeVisible();
  const note = result.getByText("导出包含全部已保存的差异，不受当前筛选影响。");
  await expect(note).toHaveCount(0);
  await result.getByRole("button", { name: /^不一致/ }).click();
  await expect(note).toBeVisible();
});

const failure = (key: string, retryStatus: string) => ({
  taskIndex: 8,
  parentTaskId: 8,
  childTaskId: "8-1",
  executionNodeIp: "10.0.1.12",
  sourceBucket: "src-data",
  destBucket: "dst-data",
  key,
  objectSize: 8,
  stage: "TRANSFER",
  errorCode: "TRANSFER_FAILED",
  errorMessage: `HeadObject ${key}: connection reset`,
  retryCount: 1,
  retryStatus,
  timestamp: 1792130530972,
});

async function exceptionRecords(page: Page) {
  await mockApi(page);
  const bodies: Record<string, unknown>[] = [];
  const localQueries: URLSearchParams[] = [];
  await page.route("**/api/master/failed", (route) => {
    const body = route.request().postDataJSON() ?? {};
    bodies.push(body);
    const rows = [
      failure("resolved.bin", "SKIPPED"),
      failure("open.bin", "FAILED"),
    ].filter(
      (row) =>
        !body.resolution ||
        (body.resolution === "resolved") ===
          ["SKIPPED", "RECOVERED"].includes(row.retryStatus),
    );
    return route.fulfill({
      json: {
        success: true,
        failedKeys: { "10.0.1.12": rows, "10.0.1.12Count": rows.length },
        totalCount: rows.length,
        clearableCount: 1,
        queriedWorkers: ["10.0.1.12"],
        complete: true,
      },
    });
  });
  await page.route("**/api/master/local-transfers/failures?**", (route) => {
    localQueries.push(new URL(route.request().url()).searchParams);
    return route.fulfill({
      json: { success: true, data: { items: [], total: 0, complete: true } },
    });
  });
  return { bodies, localQueries };
}

test("failure records show unresolved records by default", async ({ page }) => {
  const state = await exceptionRecords(page);
  await page.goto("/exceptions");
  await expect(page.getByText("open.bin").first()).toBeVisible();
  await expect(page.getByText("resolved.bin")).toHaveCount(0);
  expect(state.bodies.at(-1)).toMatchObject({ resolution: "unresolved" });
  await page.getByRole("combobox", { name: "处理状态" }).click();
  await page.getByRole("option", { name: "全部" }).click();
  await page.getByRole("button", { name: "应用筛选" }).click();
  await expect(page.getByText("resolved.bin").first()).toBeVisible();
  expect(state.bodies.at(-1)).not.toHaveProperty("resolution");
  await page.getByRole("tab", { name: "本地迁移失败" }).click();
  await expect
    .poll(() => state.localQueries.at(-1)?.get("resolution"))
    .toBe("unresolved");
  await expect(page.getByRole("tab", { name: "降级迁移记录" })).toHaveAttribute(
    "title",
    /当前版本不再产生这类记录/,
  );
});

test("narrow screens show the reason and retry result of each record", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await exceptionRecords(page);
  await page.goto("/exceptions");
  const summary = page.getByTestId("exception-summary").first();
  await expect(summary).toBeVisible();
  await expect(summary).toContainText("HeadObject open.bin: connection reset");
  await expect(summary).toContainText("再次失败");
});

test("the time filters name the server time zone", async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/worker/node-info", (route) =>
    route.fulfill({ json: { isMaster: true, timeZone: "CST (UTC+08:00)" } }),
  );
  await page.goto("/create");
  await page.getByText("迁移选项").first().click();
  await expect(
    page.getByText("时间按服务端时区（CST (UTC+08:00)）解释。").first(),
  ).toBeVisible();
});

test("container Agents get update commands and disabled updates explain why", async ({
  page,
}) => {
  await mockApi(page);
  const image = "registry.example/transfer:4.2.0";
  await page.route("**/api/master/software-updates", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          queue: { phase: "IDLE", items: [] },
          master: {
            info: { role: "master", version: "4.2.0", supported: true },
            update: { phase: "IDLE" },
          },
          agents: [
            {
              address: "agent-docker-1",
              displayName: "agent-docker-1",
              info: {
                role: "agent",
                version: "4.1.41",
                supported: false,
                reason: "CONTAINER",
              },
              update: { phase: "IDLE" },
            },
            {
              address: "agent-docker-2",
              displayName: "agent-docker-2",
              hostname: "oss-transfer-agent-1a2b3c",
              info: {
                role: "agent",
                version: "4.1.41",
                supported: false,
                reason: "CONTAINER",
              },
              update: { phase: "IDLE" },
            },
          ],
          latest: { version: "4.2.0" },
          available: false,
          agentImage: image,
        },
      },
    }),
  );
  await page.goto("/tools?tab=updates");
  await expect(page.getByText("v4.2.0", { exact: true })).toBeVisible();
  const row = page.getByRole("row").filter({ hasText: "agent-docker-1" });
  await expect(row.getByRole("cell", { name: "v4.1.41" })).toBeVisible();
  await expect(
    row.getByRole("button", { name: "更新", exact: true }),
  ).toBeDisabled();
  await row
    .getByRole("button", { name: "更新", exact: true })
    .locator("..")
    .hover();
  await expect(page.getByRole("tooltip")).toContainText(
    "Docker 部署的节点不能在线更新",
  );
  await row.getByRole("button", { name: "更新命令" }).click();
  const dialog = page.getByRole("dialog");
  const commands = containerAgentCommands(image);
  await expect(dialog.locator("pre").nth(0)).toHaveText(commands.run);
  await expect(dialog.locator("pre").nth(1)).toHaveText(commands.compose);
  expect(commands.run).toContain(
    `-v oss-transfer-agent-data:/data ${image} oss-transfer-agent --data-dir /data`,
  );
  // The recreated container keeps the host.docker.internal mapping the join
  // command may have added, or the Agent cannot reach the Master on Linux.
  expect(commands.run).toContain(
    "--restart unless-stopped --add-host host.docker.internal:host-gateway -v ",
  );
  // An Agent added with the v4.2.0 Docker command keeps its own container
  // and volume names, read from the hostname it reports.
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
  const joined = page.getByRole("row").filter({ hasText: "agent-docker-2" });
  await joined.getByRole("button", { name: "更新命令" }).click();
  const named = containerAgentCommands(image, "oss-transfer-agent-1a2b3c");
  await expect(page.getByRole("dialog").locator("pre").nth(0)).toHaveText(
    named.run,
  );
  expect(named.run).toContain("docker rm -f oss-transfer-agent-1a2b3c\n");
  expect(named.run).toContain(
    `--name oss-transfer-agent-1a2b3c --hostname oss-transfer-agent-1a2b3c `,
  );
  expect(named.run).toContain(
    `-v oss-transfer-agent-1a2b3c-data:/data ${image} oss-transfer-agent --data-dir /data`,
  );
  // Same options, in the same order, as the join command's docker run.
  expect(named.run.split("\n")[2]).toBe(
    `docker run -d --name oss-transfer-agent-1a2b3c --hostname oss-transfer-agent-1a2b3c --restart unless-stopped --add-host host.docker.internal:host-gateway --read-only --tmpfs /tmp:mode=1777 --cap-drop ALL --security-opt no-new-privileges --stop-timeout 45 -v oss-transfer-agent-1a2b3c-data:/data ${image} oss-transfer-agent --data-dir /data`,
  );
});
