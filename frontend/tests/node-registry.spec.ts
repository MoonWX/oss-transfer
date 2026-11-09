import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";
import type { AgentInvitation } from "../src/lib/nodeRegistry";

const joinUrl = "https://transfer.example/api/agents/bootstrap/test-once";
const rootShell = `sh -c 'if [ "$(id -u)" = 0 ]; then exec bash; else exec sudo bash; fi'`;
const traditionalCommand = `curl -fsSL '${joinUrl}' | ${rootShell}`;
const windowsCommand = `irm '${joinUrl}?platform=windows' | iex`;
const upstreamTraditionalCommand = `curl -fsSL '${joinUrl}?source=upstream' | ${rootShell}`;
const upstreamWindowsCommand = `irm '${joinUrl}?platform=windows&source=upstream' | iex`;
const nativeCommand = `./oss-transfer-agent --join-url '${joinUrl}'`;
const dockerEnroll = `docker run --rm -v oss-transfer-agent-abcdef-data:/data moonwx/osstransfer:4.2.0 oss-transfer-agent --join-url '${joinUrl}' --data-dir /data --enroll-only`;
const dockerRun =
  "docker run -d --name oss-transfer-agent-abcdef --restart unless-stopped -v oss-transfer-agent-abcdef-data:/data moonwx/osstransfer:4.2.0 oss-transfer-agent --data-dir /data";
const dockerCommand = `${dockerEnroll} && ${dockerRun}`;
const dockerWindowsCommand = `${dockerEnroll}; if ($LASTEXITCODE -eq 0) { ${dockerRun} }`;
const composeCommand = `curl -fsSL '${joinUrl}?method=compose' | ${rootShell}`;

async function setup(
  page: Page,
  useAgent = false,
  installCommands: Partial<AgentInvitation> = {},
) {
  await mockApi(page);
  let nodes = [
    { address: "10.0.1.12", label: "主节点", master: true },
    {
      address: "10.0.1.13",
      label: "数据节点",
      master: false,
      agent: useAgent,
      online: false,
      lastSeen: "2026-10-08T01:00:00Z",
    },
  ];
  const writes: {
    method: string;
    address?: string;
    body: any;
    internalToken?: string;
  }[] = [];
  let reject = "";
  const invitations: { body: any; internalToken?: string }[] = [];
  await page.route("**/api/master/agents/invitations/*", (route) =>
    route.fulfill({ json: { success: true, data: { status: "pending" } } }),
  );
  await page.route("**/api/master/agents/invitations", async (route) => {
    const request = route.request();
    invitations.push({
      body: request.postDataJSON(),
      internalToken: request.headers()["x-internal-token"],
    });
    await route.fulfill({
      status: reject ? 409 : 200,
      json: reject
        ? { success: false, message: reject }
        : {
            success: true,
            id: `invitation-${invitations.length}`,
            expiresAt: new Date(Date.now() + 1800000).toISOString(),
            joinUrl,
            dockerCommand,
            dockerWindowsCommand,
            composeCommand,
            traditionalCommand,
            ...installCommands,
          },
    });
  });
  await page.route("**/api/master/nodes**", async (route) => {
    const request = route.request();
    const address = new URL(request.url()).pathname.split("/")[4];
    if (request.method() !== "GET") {
      writes.push({
        method: request.method(),
        address,
        body: request.postDataJSON(),
        internalToken: request.headers()["x-internal-token"],
      });
      if (reject) {
        await route.fulfill({
          status: 409,
          json: { success: false, message: reject },
        });
        return;
      }
      if (request.method() === "DELETE")
        nodes = nodes.filter((node) => node.address !== address);
      else {
        const body = request.postDataJSON();
        nodes = nodes.filter((node) => node.address !== address);
        nodes.push({
          address: body.address,
          label: body.label,
          master: address === "10.0.1.12",
        });
      }
    }
    await route.fulfill({
      json: {
        success: true,
        data: { nodes, port: 1685, masterWebUrl: "https://transfer.example" },
      },
    });
  });
  await page.goto("/nodes");
  await expect(page.getByRole("button", { name: "编辑节点" })).toHaveCount(2);
  return {
    writes,
    invitations,
    reject: (message: string) => {
      reject = message;
    },
  };
}

test("Agent node connection state reflects its heartbeat", async ({ page }) => {
  await setup(page, true);
  const agent = page.getByRole("region", {
    name: "执行节点 数据节点 (10.0.1.13)",
  });
  await expect(agent.getByText("Agent", { exact: true })).toBeVisible();
  await expect(agent.getByText("离线", { exact: true })).toBeVisible();
  await expect(agent.getByText(/最后连接：/)).toBeVisible();
  await expect(
    page.getByText("1 / 2 个节点正常响应", { exact: true }),
  ).toBeVisible();
});

test("Agent address is immutable while its name remains editable", async ({
  page,
}) => {
  const state = await setup(page, true);
  await page
    .getByRole("button", { name: "编辑节点", exact: true })
    .nth(1)
    .click();
  const dialog = page.getByRole("dialog", { name: "编辑节点", exact: true });
  await expect(
    dialog.getByRole("textbox", { name: "节点标识", exact: true }),
  ).toHaveCount(0);
  await dialog
    .getByRole("textbox", { name: "节点名称", exact: true })
    .fill("Archive Agent");
  await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(state.writes[0].body).toEqual({
    address: "10.0.1.13",
    label: "Archive Agent",
  });
});

test("node invitation defaults to native installation and keeps Docker available without changing membership", async ({
  page,
}) => {
  const state = await setup(page);
  await page.getByRole("button", { name: "添加节点", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "添加节点", exact: true });
  await dialog
    .getByRole("textbox", { name: "节点名称", exact: true })
    .fill("Test Agent");
  await expect(dialog.getByRole("textbox")).toHaveCount(2);
  await dialog
    .getByRole("textbox", { name: "节点名称", exact: true })
    .fill("新增节点");
  await dialog
    .getByRole("button", { name: "生成加入指令", exact: true })
    .click();
  await expect(dialog.getByLabel("加入链接", { exact: true })).toHaveCount(0);
  await expect(
    dialog.getByRole("tab", { name: "传统部署", exact: true }),
  ).toHaveAttribute("aria-selected", "true");
  await expect(dialog.getByLabel("传统部署 指令", { exact: true })).toHaveValue(
    traditionalCommand,
  );
  await dialog.getByRole("tab", { name: "Docker", exact: true }).click();
  await expect(dialog.getByLabel("Docker 指令", { exact: true })).toHaveValue(
    dockerCommand,
  );
  await expect(
    dialog.getByText("访问本地文件前，请将所需目录挂载到容器。"),
  ).toBeVisible();
  await expect(dialog.getByText(/加入凭证仅可使用一次/)).toBeVisible();
  expect(state.invitations).toEqual([
    {
      body: { label: "新增节点", masterUrl: new URL(page.url()).origin },
      internalToken: undefined,
    },
  ]);
  expect(state.writes).toHaveLength(0);
  await expect(
    page.getByRole("button", { name: "编辑节点", includeHidden: true }),
  ).toHaveCount(2);
  await dialog.getByRole("button", { name: "关闭", exact: true }).click();
  await page.getByRole("button", { name: "添加节点", exact: true }).click();
  await expect(
    dialog.getByRole("textbox", { name: "节点名称", exact: true }),
  ).toHaveValue("");
  await expect(dialog.getByLabel("加入链接", { exact: true })).toHaveCount(0);
});

test("native Agent commands precede Docker and switching tabs preserves join credentials", async ({
  page,
}) => {
  const state = await setup(page, true, {
    nativeAgent: { available: true },
    windowsCommand,
    nativeCommand,
    upstreamTraditionalCommand,
    upstreamWindowsCommand,
  });
  await page.getByRole("button", { name: "添加节点", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "添加节点", exact: true });
  await dialog
    .getByRole("textbox", { name: "节点名称", exact: true })
    .fill("Test Agent");
  await dialog
    .getByRole("button", { name: "生成加入指令", exact: true })
    .click();
  await expect(dialog.getByRole("tab")).toHaveText([
    "Linux / macOS / FreeBSD",
    "Windows",
    "Docker",
  ]);
  await expect(dialog.getByRole("tab").first()).toHaveAttribute(
    "aria-selected",
    "true",
  );
  await expect(
    dialog.getByLabel("Linux / macOS / FreeBSD 指令", { exact: true }),
  ).toHaveValue(traditionalCommand);
  await dialog
    .getByRole("button", { name: "改用公网下载", exact: true })
    .click();
  await expect(
    dialog.getByLabel("Linux / macOS / FreeBSD 指令", { exact: true }),
  ).toHaveValue(upstreamTraditionalCommand);
  await dialog
    .getByRole("button", { name: "改从 Master 下载", exact: true })
    .click();
  await expect(
    dialog.getByLabel("Linux / macOS / FreeBSD 指令", { exact: true }),
  ).toHaveValue(traditionalCommand);
  await dialog.getByRole("tab", { name: "Windows", exact: true }).click();
  await expect(dialog.getByLabel("Windows 指令", { exact: true })).toHaveValue(
    windowsCommand,
  );
  await dialog
    .getByRole("button", { name: "改用公网下载", exact: true })
    .click();
  await expect(dialog.getByLabel("Windows 指令", { exact: true })).toHaveValue(
    upstreamWindowsCommand,
  );
  await dialog
    .getByRole("button", { name: "改从 Master 下载", exact: true })
    .click();
  await expect(dialog.getByLabel("Windows 指令", { exact: true })).toHaveValue(
    windowsCommand,
  );
  await expect(
    dialog.getByRole("tab", { name: "直接运行", exact: true }),
  ).toHaveCount(0);
  await dialog.getByRole("tab", { name: "Docker", exact: true }).click();
  await expect(dialog.getByLabel("Docker 指令", { exact: true })).toHaveValue(
    dockerCommand,
  );
  await expect(
    dialog.getByRole("button", { name: /改用公网下载|改从 Master 下载/ }),
  ).toHaveCount(0);
  await dialog
    .getByRole("tab", { name: "Linux / macOS / FreeBSD", exact: true })
    .click();
  await expect(
    dialog.getByLabel("Linux / macOS / FreeBSD 指令", { exact: true }),
  ).toHaveValue(traditionalCommand);
  expect(state.invitations).toHaveLength(1);
  expect(state.writes).toHaveLength(0);
});

for (const [method, commands] of [
  ["Linux / macOS / FreeBSD", { traditionalCommand }],
  ["Windows", { windowsCommand }],
  ["直接运行", { nativeCommand }],
] as const) {
  test(`native-only invitation supports ${method} without a Docker command`, async ({
    page,
  }) => {
    await setup(page, true, {
      dockerCommand: undefined,
      dockerWindowsCommand: undefined,
      composeCommand: undefined,
      traditionalCommand: undefined,
      nativeAgent: { available: true },
      ...commands,
    });
    await page.getByRole("button", { name: "添加节点", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "添加节点", exact: true });
    await dialog
      .getByRole("textbox", { name: "节点名称", exact: true })
      .fill("Test Agent");
    await dialog
      .getByRole("button", { name: "生成加入指令", exact: true })
      .click();
    await expect(dialog.getByRole("tab")).toHaveText([method]);
    await expect(dialog.getByRole("tab")).toHaveAttribute(
      "aria-selected",
      "true",
    );
    await expect(
      dialog.getByLabel(`${method} 指令`, { exact: true }),
    ).toHaveValue(Object.values(commands)[0]);
    await expect(dialog.getByRole("alert")).toHaveCount(0);
  });
}

test("invitation generation supports automatic address and preserves errors", async ({
  page,
}) => {
  const state = await setup(page);
  state.reject("无法生成加入链接，请检查主节点地址");
  await page.getByRole("button", { name: "添加节点", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "添加节点", exact: true });
  await dialog
    .getByRole("textbox", { name: "节点名称", exact: true })
    .fill("Test Agent");
  await dialog
    .getByRole("textbox", { name: "节点名称", exact: true })
    .fill("Agent 1");
  await dialog
    .getByRole("button", { name: "生成加入指令", exact: true })
    .click();
  // The dev server origin is a loopback address, so its warning is shown too.
  await expect(
    dialog.getByRole("alert").filter({ hasText: "无法生成加入链接" }),
  ).toBeVisible();
  await expect(
    dialog.getByRole("textbox", { name: "节点名称", exact: true }),
  ).toHaveValue("Agent 1");
  state.reject("");
  await dialog
    .getByRole("button", { name: "生成加入指令", exact: true })
    .click();
  await expect(
    dialog.getByLabel("传统部署 指令", { exact: true }),
  ).toBeVisible();
  expect(state.invitations[1].body).toEqual({
    label: "Agent 1",
    masterUrl: new URL(page.url()).origin,
  });
});

test("Agent invitation dialog fits mobile screens and copies using HTTP fallback", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await setup(page);
  await page.evaluate(() => {
    localStorage.setItem("oss-transfer-theme", "dark");
    Object.defineProperty(navigator, "clipboard", {
      value: undefined,
      configurable: true,
    });
    (window as any).copiedTexts = [];
    document.execCommand = (command) => {
      if (command === "copy")
        (window as any).copiedTexts.push(
          (document.activeElement as HTMLTextAreaElement).value,
        );
      return true;
    };
  });
  await page.getByRole("button", { name: "添加节点", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "添加节点", exact: true });
  await dialog
    .getByRole("textbox", { name: "节点名称", exact: true })
    .fill("Test Agent");
  await dialog
    .getByRole("button", { name: "生成加入指令", exact: true })
    .click();
  await dialog.getByRole("button", { name: "复制指令", exact: true }).click();
  const copies = await page.evaluate(() => (window as any).copiedTexts);
  expect(copies).toHaveLength(1);
  expect(copies[0]).toBe(traditionalCommand);
  const bounds = await dialog.boundingBox();
  expect(bounds!.x).toBeGreaterThanOrEqual(0);
  expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(390);
  expect(
    await dialog.evaluate(
      (element) => element.scrollWidth <= element.clientWidth,
    ),
  ).toBe(true);
});

test("master address cannot be changed while its label can be saved", async ({
  page,
}) => {
  const state = await setup(page);
  await page
    .getByRole("button", { name: "编辑节点", exact: true })
    .first()
    .click();
  const dialog = page.getByRole("dialog", { name: "编辑节点", exact: true });
  await expect(
    dialog.getByRole("textbox", { name: "节点地址", exact: true }),
  ).toBeDisabled();
  await expect(dialog.getByRole("textbox")).toHaveCount(2);
  await expect(dialog.getByText(/展示目录|访问白名单/)).toHaveCount(0);
  await dialog
    .getByRole("textbox", { name: "节点名称", exact: true })
    .fill("主控节点");
  await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(state.writes[0]).toEqual({
    method: "PUT",
    address: "10.0.1.12",
    body: {
      address: "10.0.1.12",
      label: "主控节点",
    },
    internalToken: undefined,
  });
});

test("node removal requires confirmation and retains inaccessible or referenced nodes", async ({
  page,
}) => {
  const state = await setup(page);
  await expect(
    page.getByRole("button", { name: "移除节点", exact: true }),
  ).toHaveCount(1);
  await page.getByRole("button", { name: "移除节点", exact: true }).click();
  const dialog = page.getByRole("dialog", {
    name: "移除节点 数据节点 (10.0.1.13)",
    exact: true,
  });
  await expect(dialog.getByText(/文件、对象和数据库不会被删除/)).toBeVisible();
  // Results stored on the node are lost with it: say so and suggest exporting.
  await expect(
    dialog
      .getByRole("alert")
      .filter({ hasText: "任务报告会不完整，比对记录会消失" })
      .filter({ hasText: "请先导出任务报告和比对结果" }),
  ).toBeVisible();
  // The precondition the Master really checks.
  await expect(
    dialog.getByText(/有未结束的任务使用此节点时无法移除/),
  ).toBeVisible();
  await expect(dialog.getByText(/节点不可达|仍有任务记录/)).toHaveCount(0);
  await dialog.getByRole("button", { name: "取消", exact: true }).click();
  expect(state.writes).toHaveLength(0);
  state.reject("节点仍被回收站父任务引用，请先处理任务记录");
  await page.getByRole("button", { name: "移除节点", exact: true }).click();
  await dialog.getByRole("button", { name: "确认移除", exact: true }).click();
  await expect(
    dialog.getByRole("alert").filter({ hasText: "节点仍被回收站父任务引用" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", {
      name: "移除节点",
      exact: true,
      includeHidden: true,
    }),
  ).toHaveCount(1);
  state.reject("");
  await dialog.getByRole("button", { name: "确认移除", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "移除节点", exact: true }),
  ).toHaveCount(0);
  expect(
    state.writes.every(
      (request) =>
        request.method === "DELETE" && request.address === "10.0.1.13",
    ),
  ).toBe(true);
});

test("worker address and name remain editable without sending display directories", async ({
  page,
}) => {
  const state = await setup(page);
  await page
    .getByRole("button", { name: "编辑节点", exact: true })
    .nth(1)
    .click();
  const dialog = page.getByRole("dialog", { name: "编辑节点", exact: true });
  await dialog
    .getByRole("textbox", { name: "节点地址", exact: true })
    .fill("10.0.1.14");
  await dialog
    .getByRole("textbox", { name: "节点名称", exact: true })
    .fill("归档节点");
  await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(state.writes[0]).toEqual({
    method: "PUT",
    address: "10.0.1.13",
    body: { address: "10.0.1.14", label: "归档节点" },
    internalToken: undefined,
  });
  await expect(
    page.getByText("归档节点 (10.0.1.14)", { exact: true }),
  ).toBeVisible();
});

async function openInvitation(page: Page, label = "新节点") {
  await page.getByRole("button", { name: "添加节点", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "添加节点", exact: true });
  await dialog
    .getByRole("textbox", { name: "节点名称", exact: true })
    .fill(label);
  await dialog
    .getByRole("button", { name: "生成加入指令", exact: true })
    .click();
  await expect(
    dialog.getByRole("button", { name: "复制指令", exact: true }),
  ).toBeVisible();
  return dialog;
}

test("invitation closes only when its node joins and refreshes the node list", async ({
  page,
}) => {
  // setup() navigates, so pause polling first; only the join may refresh nodes.
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "0"),
  );
  await setup(page, true);
  let joined = false;
  let checks = 0;
  let refreshes = 0;
  page.on("request", (request) => {
    if (new URL(request.url()).pathname === "/api/master/nodes") refreshes++;
  });
  await page.route("**/api/master/agents/invitations/*", (route) => {
    checks++;
    return route.fulfill({
      json: {
        success: true,
        data: joined
          ? { status: "joined", nodeId: "new-agent" }
          : { status: "pending" },
      },
    });
  });
  const dialog = await openInvitation(page);
  await expect.poll(() => checks).toBeGreaterThan(0);
  await expect(dialog).toBeVisible();
  const beforeJoin = refreshes;
  joined = true;
  await expect(dialog).toBeHidden();
  await expect(page.getByText("节点已添加。", { exact: true })).toBeVisible();
  await expect.poll(() => refreshes).toBeGreaterThan(beforeJoin);
});

for (const status of ["expired", "revoked"]) {
  test(`invitation stops checking after ${status} without claiming success`, async ({
    page,
  }) => {
    await setup(page);
    let checks = 0;
    await page.route("**/api/master/agents/invitations/*", (route) => {
      checks++;
      return route.fulfill({ json: { success: true, data: { status } } });
    });
    const dialog = await openInvitation(page);
    await expect.poll(() => checks).toBe(1);
    await page.waitForTimeout(2300);
    expect(checks).toBe(1);
    await expect(dialog).toBeVisible();
    await expect(page.getByText("节点已添加。", { exact: true })).toHaveCount(
      0,
    );
  });
}

test("a slow enrollment check does not overlap or close a later invitation", async ({
  page,
}) => {
  await setup(page);
  let finish!: () => void;
  const delayed = new Promise<void>((resolve) => {
    finish = resolve;
  });
  let oldChecks = 0;
  let newChecks = 0;
  await page.route("**/api/master/agents/invitations/*", async (route) => {
    if (route.request().url().endsWith("/invitation-1")) {
      oldChecks++;
      await delayed;
      await route
        .fulfill({
          json: {
            success: true,
            data: { status: "joined", nodeId: "old-agent" },
          },
        })
        .catch(() => {});
    } else {
      newChecks++;
      await route.fulfill({
        json: { success: true, data: { status: "pending" } },
      });
    }
  });
  const first = await openInvitation(page);
  await expect.poll(() => oldChecks).toBe(1);
  await page.waitForTimeout(2300);
  expect(oldChecks).toBe(1);
  await first.getByRole("button", { name: "关闭", exact: true }).click();
  const second = await openInvitation(page, "另一个节点");
  await expect.poll(() => newChecks).toBeGreaterThan(0);
  finish();
  await page.waitForTimeout(300);
  await expect(second).toBeVisible();
  await expect(page.getByText("节点已添加。", { exact: true })).toHaveCount(0);
});

test("invitation retries temporary status failures until the node joins", async ({
  page,
}) => {
  await setup(page);
  let checks = 0;
  await page.route("**/api/master/agents/invitations/*", (route) => {
    checks++;
    return checks === 1
      ? route.fulfill({
          status: 503,
          json: { success: false, message: "temporary outage" },
        })
      : route.fulfill({
          json: {
            success: true,
            data: { status: "joined", nodeId: "new-agent" },
          },
        });
  });
  const dialog = await openInvitation(page);
  await expect(dialog).toBeHidden();
  expect(checks).toBe(2);
});

test("Docker join offers Docker Desktop, PowerShell and Linux Compose commands and names the download source", async ({
  page,
}) => {
  const state = await setup(page, true, {
    nativeAgent: { available: true },
    windowsCommand,
    upstreamTraditionalCommand,
    upstreamWindowsCommand,
    downloadSource: { kind: "master" },
    upstreamDownloadSource: { kind: "auto" },
  });
  // Registered after the generic API mock so that this route takes precedence.
  await page.route("**/api/master/network", (route) =>
    route.fulfill({
      json: { success: true, data: { lanAddresses: ["192.168.1.20"] } },
    }),
  );
  await page.getByRole("button", { name: "添加节点", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "添加节点", exact: true });
  await dialog
    .getByRole("textbox", { name: "节点名称", exact: true })
    .fill("Docker Agent");
  const address = dialog.getByRole("textbox", {
    name: "Master 地址（Agent 需能访问）",
  });
  await address.fill("http://localhost:1685");
  const warning = dialog.getByText(/这是本机回环地址/);
  await expect(warning).toBeVisible();
  await dialog
    .getByRole("button", { name: "192.168.1.20（局域网地址）", exact: true })
    .click();
  await expect(address).toHaveValue("http://192.168.1.20:1685");
  await expect(warning).toHaveCount(0);
  await address.fill("http://127.0.0.1:1685/");
  await dialog
    .getByRole("button", {
      name: "host.docker.internal（Docker 与 Master 在同一台主机）",
      exact: true,
    })
    .click();
  await expect(address).toHaveValue("http://host.docker.internal:1685");
  await dialog
    .getByRole("button", { name: "生成加入指令", exact: true })
    .click();
  expect(state.invitations[0].body).toEqual({
    label: "Docker Agent",
    masterUrl: "http://host.docker.internal:1685",
  });
  await expect(
    dialog.getByText("下载来源：本 Master，新节点无需访问公网。"),
  ).toBeVisible();
  await dialog
    .getByRole("button", { name: "改用公网下载", exact: true })
    .click();
  await expect(
    dialog.getByText(/下载来源：GitHub 或京东云 OSS.*ipinfo\.io/),
  ).toBeVisible();
  await dialog.getByRole("tab", { name: "Docker", exact: true }).click();
  const commandField = dialog.getByLabel("Docker 指令", { exact: true });
  await expect(commandField).toHaveValue(dockerCommand);
  await expect(dialog.getByText(/下载来源/)).toHaveCount(0);
  await expect(
    dialog.getByRole("button", { name: "macOS / Linux", exact: true }),
  ).toHaveAttribute("aria-pressed", "true");
  await expect(dialog.getByText(/无需 root/)).toBeVisible();
  await dialog
    .getByRole("button", { name: "Windows PowerShell", exact: true })
    .click();
  await expect(commandField).toHaveValue(dockerWindowsCommand);
  await dialog
    .getByRole("button", { name: "Linux Docker Compose", exact: true })
    .click();
  await expect(commandField).toHaveValue(composeCommand);
  await expect(
    dialog.getByText(/以 root 安装 Docker Compose 项目/),
  ).toBeVisible();
  await expect(dialog.getByText(/Docker 容器内的 Agent 无法访问/)).toHaveCount(
    0,
  );
});

test("Docker tab sends a loopback Master address back for correction", async ({
  page,
}) => {
  const loopbackJoin = "http://localhost:1685/api/agents/bootstrap/test-once";
  const state = await setup(page, true, { joinUrl: loopbackJoin });
  await page.getByRole("button", { name: "添加节点", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "添加节点", exact: true });
  await dialog
    .getByRole("textbox", { name: "节点名称", exact: true })
    .fill("Loopback Agent");
  await dialog
    .getByRole("textbox", { name: "Master 地址（Agent 需能访问）" })
    .fill("http://localhost:1685");
  await dialog
    .getByRole("button", { name: "生成加入指令", exact: true })
    .click();
  await dialog.getByRole("tab", { name: "Docker", exact: true }).click();
  await expect(
    dialog.getByText(
      "Master 地址是本机回环地址，Docker 容器内的 Agent 无法访问。请修改地址后重新生成。",
    ),
  ).toBeVisible();
  await dialog
    .getByRole("button", { name: "修改 Master 地址", exact: true })
    .click();
  await expect(
    dialog.getByRole("textbox", { name: "节点名称", exact: true }),
  ).toHaveValue("Loopback Agent");
  await expect(
    dialog.getByRole("textbox", { name: "Master 地址（Agent 需能访问）" }),
  ).toHaveValue("http://localhost:1685");
  expect(state.invitations).toHaveLength(1);
});
