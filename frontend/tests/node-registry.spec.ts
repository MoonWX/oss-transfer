import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";
import type { AgentInvitation } from "../src/lib/nodeRegistry";

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
            id: "invitation-1",
            expiresAt: new Date(Date.now() + 1800000).toISOString(),
            joinUrl: "https://transfer.example/agent/join/test-once",
            dockerCommand:
              "docker run --rm moonwx/osstransfer:4.0.0 agent --join https://transfer.example/agent/join/test-once",
            traditionalCommand:
              "curl -fsSL https://transfer.example/agent/install/test-once -o agent-install.sh && sh agent-install.sh",
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
  const agent = page.getByRole("region", { name: "执行节点 10.0.1.13" });
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
  await expect(dialog.getByRole("textbox")).toHaveCount(1);
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
    /agent-install.sh/,
  );
  await dialog.getByRole("tab", { name: "Docker", exact: true }).click();
  await expect(dialog.getByLabel("Docker 指令", { exact: true })).toHaveValue(
    /moonwx\/osstransfer/,
  );
  await expect(
    dialog.getByText("访问本地文件前，请将所需目录挂载到容器。"),
  ).toBeVisible();
  await expect(dialog.getByText(/加入凭证仅可使用一次/)).toBeVisible();
  expect(state.invitations).toEqual([
    {
      body: { label: "新增节点" },
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
  const windowsCommand =
    "powershell -File Install.ps1 -JoinUrl https://transfer.example/agent/join/test-once";
  const nativeCommand =
    "./oss-transfer-agent --join-url https://transfer.example/agent/join/test-once";
  const state = await setup(page, true, {
    nativeAgent: { available: true },
    windowsCommand,
    nativeCommand,
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
    "直接运行",
    "Docker",
  ]);
  await expect(dialog.getByRole("tab").first()).toHaveAttribute(
    "aria-selected",
    "true",
  );
  await expect(
    dialog.getByLabel("Linux / macOS / FreeBSD 指令", { exact: true }),
  ).toHaveValue(/agent-install.sh/);
  await dialog.getByRole("tab", { name: "Windows", exact: true }).click();
  await expect(dialog.getByLabel("Windows 指令", { exact: true })).toHaveValue(
    windowsCommand,
  );
  await dialog.getByRole("tab", { name: "直接运行", exact: true }).click();
  await expect(dialog.getByLabel("直接运行 指令", { exact: true })).toHaveValue(
    nativeCommand,
  );
  await dialog.getByRole("tab", { name: "Docker", exact: true }).click();
  await dialog
    .getByRole("tab", { name: "Linux / macOS / FreeBSD", exact: true })
    .click();
  await expect(
    dialog.getByLabel("Linux / macOS / FreeBSD 指令", { exact: true }),
  ).toHaveValue(/agent-install.sh/);
  expect(state.invitations).toHaveLength(1);
  expect(state.writes).toHaveLength(0);
});

for (const [method, commands] of [
  [
    "Linux / macOS / FreeBSD",
    { traditionalCommand: "native-install --token test-once" },
  ],
  ["Windows", { windowsCommand: "Install.ps1 -Token test-once" }],
  ["直接运行", { nativeCommand: "./oss-transfer-agent --token test-once" }],
] as const) {
  test(`native-only invitation supports ${method} without a Docker command`, async ({
    page,
  }) => {
    await setup(page, true, {
      dockerCommand: undefined,
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
  await expect(dialog.getByRole("alert")).toContainText("无法生成加入链接");
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
  expect(state.invitations[1].body).toEqual({ label: "Agent 1" });
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
  expect(copies[0]).toContain("agent-install.sh");
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
    name: "移除节点 数据节点",
    exact: true,
  });
  await expect(
    dialog.getByText(/不删除节点上的文件、对象或数据库/),
  ).toBeVisible();
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
  await expect(page.getByText("归档节点", { exact: true })).toBeVisible();
});
