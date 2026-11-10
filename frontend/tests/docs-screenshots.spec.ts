import { expect, test, type Locator, type Page } from "@playwright/test";
import { mockApi, storageConfigs } from "./fixtures";
import {
  historyItem,
  pageOf,
  progress,
  routeComparisons,
  rows,
} from "./object-comparison-fixtures";

// Generates the annotated console screenshots under docs/images. It does not
// run with the test suite; refresh the images with:
//   DOC_SHOTS=1 CI=1 npx playwright test tests/docs-screenshots.spec.ts
test.skip(!process.env.DOC_SHOTS, "set DOC_SHOTS=1 to refresh docs/images");
test.use({ viewport: { width: 1280, height: 900 }, deviceScaleFactor: 2 });

const locales = [
  { locale: "zh-CN", suffix: "zh-CN" },
  { locale: "en-US", suffix: "en" },
] as const;
type Locale = (typeof locales)[number]["locale"];

const text = {
  addNode: { "zh-CN": "添加节点", "en-US": "Add node" },
  nodeName: { "zh-CN": "节点名称", "en-US": "Node name" },
  generate: { "zh-CN": "生成加入指令", "en-US": "Generate join commands" },
  copy: { "zh-CN": "复制指令", "en-US": "Copy command" },
  manual: { "zh-CN": "手动运行", "en-US": "Run manually" },
  script: { "zh-CN": "管理脚本", "en-US": "Management script" },
  testConnection: { "zh-CN": "测试连接", "en-US": "Test connection" },
  checkBucket: { "zh-CN": "检查指定存储桶", "en-US": "Check a bucket" },
  check: { "zh-CN": "检查", "en-US": "Check" },
  recreate: { "zh-CN": "重建命令", "en-US": "Recreate commands" },
  filter: { "zh-CN": "按结果筛选", "en-US": "Filter by result" },
  exportZip: {
    "zh-CN": "导出全部差异（ZIP）",
    "en-US": "Export all differences (ZIP)",
  },
};

const joinUrl =
  "http://10.20.0.10:1685/api/agents/bootstrap/Q7nRk2-xVd4LpTz8cYwF0aH3sJmU5eBq";
const image = "moonwx/osstransfer:4.1.42";
const dockerEnroll = `docker run --rm -v oss-transfer-agent-3fa9c1-data:/data ${image} oss-transfer-agent --join-url '${joinUrl}' --data-dir /data --enroll-only`;
const dockerRun = `docker run -d --name oss-transfer-agent-3fa9c1 --hostname oss-transfer-agent-3fa9c1 --restart unless-stopped --read-only --tmpfs /tmp:mode=1777 --cap-drop ALL --security-opt no-new-privileges --stop-timeout 45 -v oss-transfer-agent-3fa9c1-data:/data ${image} oss-transfer-agent --data-dir /data`;

async function open(page: Page, locale: Locale) {
  await page.addInitScript((value) => {
    localStorage.setItem("oss-transfer-language", value);
  }, locale);
  await mockApi(page);
}

/** Outlines targets in red with numbered badges, as the README images do. */
async function annotate(page: Page, targets: Locator[]) {
  const boxes = [];
  for (const target of targets) boxes.push(await target.boundingBox());
  await page.evaluate((boxes) => {
    document.querySelectorAll("[data-doc-mark]").forEach((e) => e.remove());
    boxes.forEach((box, index) => {
      if (!box) return;
      const pad = 6;
      const frame = document.createElement("div");
      const badge = document.createElement("div");
      frame.dataset.docMark = badge.dataset.docMark = "1";
      Object.assign(frame.style, {
        position: "fixed",
        left: `${box.x - pad}px`,
        top: `${box.y - pad}px`,
        width: `${box.width + 2 * pad}px`,
        height: `${box.height + 2 * pad}px`,
        border: "3px solid #e53935",
        borderRadius: "14px",
        boxShadow: "0 0 0 2px rgba(255,255,255,.85)",
        zIndex: "2147483646",
        pointerEvents: "none",
      });
      badge.textContent = String(index + 1);
      Object.assign(badge.style, {
        position: "fixed",
        left: `${Math.max(4, box.x - pad - 15)}px`,
        top: `${Math.max(4, box.y - pad - 15)}px`,
        width: "30px",
        height: "30px",
        borderRadius: "50%",
        background: "#e53935",
        color: "#fff",
        font: "600 16px system-ui, sans-serif",
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
        border: "2px solid #fff",
        zIndex: "2147483647",
        pointerEvents: "none",
      });
      document.body.append(frame, badge);
    });
  }, boxes);
}

/** Saves the area of `area` with a margin that keeps the badges in view. */
async function save(page: Page, area: Locator, name: string, suffix: string) {
  const box = await area.boundingBox();
  if (!box) throw new Error(`${name}: nothing to capture`);
  const viewport = page.viewportSize()!;
  const margin = 28;
  const x = Math.max(0, box.x - margin);
  const y = Math.max(0, box.y - margin / 2);
  await page.screenshot({
    path: `../docs/images/${name}.${suffix}.png`,
    clip: {
      x,
      y,
      width: Math.min(viewport.width - x, box.width + 2 * margin),
      height: Math.min(viewport.height - y, box.height + margin),
    },
  });
}

async function invitationDialog(page: Page, locale: Locale) {
  await open(page, locale);
  await page.route("**/api/master/agents/invitations/*", (route) =>
    route.fulfill({ json: { success: true, data: { status: "pending" } } }),
  );
  await page.route("**/api/master/agents/invitations", (route) =>
    route.fulfill({
      json: {
        success: true,
        id: "invitation-1",
        expiresAt: new Date(Date.now() + 1800000).toISOString(),
        joinUrl,
        nativeAgent: { available: true },
        traditionalCommand: `curl -fsSL '${joinUrl}' | bash`,
        windowsCommand: `irm '${joinUrl}?platform=windows' | iex`,
        dockerCommand: `${dockerEnroll} && ${dockerRun}`,
        dockerWindowsCommand: `${dockerEnroll}; if ($LASTEXITCODE -eq 0) { ${dockerRun} }`,
        composeCommand: `curl -fsSL '${joinUrl}?method=compose' | bash`,
        downloadSource: { kind: "master" },
      },
    }),
  );
  await page.route("**/api/master/nodes**", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          nodes: [{ address: "10.20.0.10", label: "master", master: true }],
          port: 1685,
          masterWebUrl: "http://10.20.0.10:1685",
        },
      },
    }),
  );
  await page.goto("/nodes");
  await page
    .getByRole("button", { name: text.addNode[locale], exact: true })
    .first()
    .click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel(text.nodeName[locale]).fill("agent-02");
  await dialog
    .getByRole("textbox", { name: /Master/ })
    .fill("http://10.20.0.10:1685");
  await dialog
    .getByRole("button", { name: text.generate[locale], exact: true })
    .click();
  await expect(dialog.getByRole("tab").first()).toBeVisible();
  return dialog;
}

for (const { locale, suffix } of locales) {
  test(`join commands (${locale})`, async ({ page }) => {
    const dialog = await invitationDialog(page, locale);
    const field = dialog.locator(".MuiTextField-root").last();
    await page.waitForTimeout(400);
    await annotate(page, [
      field,
      dialog.getByRole("button", { name: text.copy[locale], exact: true }),
    ]);
    await save(page, dialog, "join-command", suffix);

    await dialog.getByRole("tab", { name: text.manual[locale] }).click();
    await page.waitForTimeout(400);
    await annotate(page, [
      dialog.getByRole("tab", { name: text.manual[locale] }),
      dialog.getByRole("group"),
      field,
      dialog.getByTestId("copy-join-url"),
    ]);
    await save(page, dialog, "manual-run", suffix);

    await dialog.getByRole("tab", { name: "Docker", exact: true }).click();
    await page.waitForTimeout(400);
    await annotate(page, [dialog.getByRole("group"), field]);
    await save(page, dialog, "docker-join", suffix);
  });

  test(`bucket permission check (${locale})`, async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 1500 });
    await open(page, locale);
    await page.route("**/api/master/s3-config/*/buckets**", (route) =>
      route.fulfill({
        json: {
          success: true,
          buckets: ["archive", "photos", "photos-backup"],
          pathStyle: false,
          pathStyleAuto: true,
          checkedBucket: "archive",
        },
      }),
    );
    await page.route("**/api/master/s3-config/*/permissions", (route) =>
      route.fulfill({
        json: {
          success: true,
          bucket: "photos-backup",
          write: true,
          node: "agent-01 (10.20.0.11)",
          results: [
            { operation: "LIST", status: "PASSED" },
            { operation: "HEAD", status: "PASSED", key: "2026/a.jpg" },
            { operation: "GET", status: "PASSED", key: "2026/a.jpg" },
            { operation: "PUT", status: "PASSED" },
            { operation: "HEAD_WRITTEN", status: "PASSED" },
            { operation: "MULTIPART", status: "PASSED" },
            { operation: "DELETE_PROBE", status: "PASSED" },
          ],
        },
      }),
    );
    await page.goto("/storage");
    await page
      .getByRole("button", { name: text.testConnection[locale], exact: true })
      .first()
      .click();
    const dialog = page.getByRole("dialog");
    await dialog.getByLabel(text.checkBucket[locale]).fill("photos-backup");
    await dialog.getByRole("checkbox").check();
    await dialog
      .getByRole("button", { name: text.check[locale], exact: true })
      .click();
    await expect(dialog.getByTestId("permission-summary")).toBeVisible();
    await page.waitForTimeout(300);
    await annotate(page, [
      dialog.locator(".MuiTextField-root").last(),
      dialog.locator(".MuiFormControlLabel-root"),
      dialog.getByTestId("permission-summary"),
    ]);
    await save(page, dialog, "permission-check", suffix);
  });

  test(`object comparison (${locale})`, async ({ page }) => {
    await open(page, locale);
    const done = {
      ...progress,
      compared: 128406,
      matched: 128402,
      rows: 4,
      // About 600 objects per second, as measured on the test environment.
      elapsedMillis: 214000,
      updatedAt: "2026-09-22T11:03:34Z",
      endedAt: "2026-09-22T11:03:34Z",
    };
    await routeComparisons(page, {
      list: () => ({ items: [historyItem(done)], complete: true }),
      page: (_, query) => pageOf(done, rows, query),
    });
    await page.setViewportSize({ width: 1280, height: 1600 });
    await page.goto("/tools?comparison=comparison-1");
    const filter = page.getByRole("group", { name: text.filter[locale] });
    await expect(filter).toBeVisible();
    // The selected comparison's results: summary, filters, differences.
    const area = page.locator(".MuiPaper-root").filter({ has: filter }).last();
    await area.evaluate((node) => node.scrollIntoView({ block: "start" }));
    await page.waitForTimeout(400);
    await annotate(page, [
      filter,
      page.getByRole("button", { name: text.exportZip[locale] }),
    ]);
    await save(page, area, "comparison", suffix);
  });

  test(`container Agent recreate commands (${locale})`, async ({ page }) => {
    await open(page, locale);
    await page.route("**/api/master/software-updates", (route) =>
      route.fulfill({
        json: {
          success: true,
          data: {
            queue: { phase: "IDLE", items: [] },
            master: {
              info: { role: "master", version: "4.1.42", supported: true },
              update: { phase: "IDLE" },
            },
            agents: [
              {
                address: "agent-docker-1",
                displayName: "agent-docker-1",
                hostname: "oss-transfer-agent-3fa9c1",
                info: {
                  role: "agent",
                  version: "4.1.42",
                  supported: false,
                  reason: "CONTAINER",
                },
                update: { phase: "IDLE" },
              },
            ],
            latest: { version: "4.1.42" },
            available: false,
            agentImage: image,
          },
        },
      }),
    );
    await page.goto("/tools?tab=updates");
    const button = page.getByRole("button", {
      name: text.recreate[locale],
      exact: true,
    });
    await button.click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.locator("pre").first()).toBeVisible();
    await page.waitForTimeout(400);
    await annotate(page, [dialog.locator("pre").first()]);
    await save(page, dialog, "docker-recreate", suffix);
  });
}

// Keeps the fixture import used when only some tests run.
void storageConfigs;
