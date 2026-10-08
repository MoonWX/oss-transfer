import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

const nodeIp = "10.0.1.12";
const eventId = "details-process:1";
const incidentId = "details-process:incident-1";
const detailsPath = `/nodes/adjustments?workerIp=${nodeIp}&eventId=${encodeURIComponent(eventId)}`;
const requestKey = "归档/2026/季度报告 <原件>/数据文件.bin";
const startedAt = 1790071200000;

function change(sequence: number, value: number, reasonCode: string) {
  return {
    id: `details-process:${sequence}`,
    processId: "details-process",
    sequence,
    incidentId,
    type: "put",
    manual: false,
    previousValue: sequence === 1 ? 1000 : sequence === 2 ? 700 : 840,
    value,
    manualTarget: 1000,
    reasonCode,
    timestamp: startedAt + (sequence - 1) * 10000,
    consecutive503Count: 20,
    consecutive503Threshold: 20,
    factor: reasonCode === "OSS_503" ? 0.7 : 1.2,
    minQps: 200,
    triggerRequest: {
      operation: "UPLOAD_PART",
      method: "PUT",
      taskId: 1048,
      bucket: "backup-archive",
      key: requestKey,
      connectionAlias: "backup",
      sourceBucket: "source-archive",
      destinationBucket: "backup-archive",
      partNumber: 3,
      attempt: 2,
      statusCode: 503,
      errorCode: "ServiceUnavailable",
      requestId: "req-503-details-001",
      extendedRequestId: "extended-request-details-001",
      message: "服务暂不可用",
      contextSource: "SDK_REQUEST",
      handlingStage: "UPLOAD_PART_RETRY",
    },
  };
}

function detailResponse() {
  const trigger = change(1, 700, "OSS_503");
  return {
    success: true,
    workerIp: nodeIp,
    event: trigger,
    incidentId,
    triggerEvent: trigger,
    events: [trigger, change(2, 840, "RECOVERY_STEP")],
    nextCursor: null as string | null,
    done: true,
    processRestarted: false,
    incidentActive: true,
  };
}

function worker(active = true) {
  const event = change(1, 700, "OSS_503");
  return {
    success: true,
    rateLimit: {
      put: {
        limitQps: active ? 700 : 1000,
        manualLimitQps: 1000,
        configuredLimitQps: 1000,
        currentQps: 20,
        limitSource: "CONFIG",
        state: active ? "REDUCED" : "NORMAL",
        lastAdjustment: active ? event : null,
      },
      head: {
        limitQps: 2000,
        manualLimitQps: 2000,
        configuredLimitQps: 2000,
        currentQps: 2,
        limitSource: "CONFIG",
        state: "NORMAL",
      },
      autoAdjust: {
        processId: "details-process",
        sequence: active ? 1 : 0,
        events: active ? [event] : [],
        observedAt: Date.now(),
        intervalMs: 10000,
        increaseFactor: 1.2,
        reduceFactor: 0.7,
        minQps: 200,
        consecutive503Count: active ? 20 : 0,
        consecutive503Threshold: 20,
        nextRecoveryAt: Date.now() + 10000,
      },
    },
  };
}

async function setup(page: Page, active = true) {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "3600"),
  );
  const state = {
    response: detailResponse() as any,
    nextPage: null as any,
    error: null as string | null,
    calls: [] as URL[],
  };
  await page.route("**/api/master/worker-status", (route) =>
    route.fulfill({
      json: { success: true, workers: { [nodeIp]: worker(active) } },
    }),
  );
  await page.route("**/api/master/rate-limit/adjustment?*", (route) => {
    const url = new URL(route.request().url());
    state.calls.push(url);
    if (state.error)
      return route.fulfill({
        status: 503,
        json: { success: false, message: state.error },
      });
    return route.fulfill({
      json: url.searchParams.has("cursor")
        ? (state.nextPage ?? state.response)
        : state.response,
    });
  });
  return state;
}

test("normal nodes have no automatic-adjustment explanation or warning", async ({
  page,
}) => {
  await setup(page, false);
  await page.goto("/nodes");
  await expect(
    page.getByRole("heading", { name: "执行节点", exact: true }),
  ).toBeVisible();
  await expect(page.getByTestId(`rate-${nodeIp}-put`)).toBeVisible();
  await expect(page.getByText("自动调节依据", { exact: true })).toHaveCount(0);
  await expect(page.getByText(/可能遭遇.*限速/)).toHaveCount(0);
  await expect(page.getByText(/每 .*秒尝试提高/)).toHaveCount(0);
});

test("a real adjustment warning opens request details without replacing the shell", async ({
  page,
}) => {
  const state = await setup(page);
  await page.goto("/tasks");
  const warning = page.getByRole("alert").filter({ hasText: /可能遭遇.*限速/ });
  await expect(warning).toBeVisible();
  await expect(warning).toHaveClass(/MuiAlert-standardWarning/);
  await expect(warning).not.toContainText("503 计数");
  await expect(warning).not.toContainText("阈值");
  const toolbar = await page
    .getByRole("button", { name: "账号设置" })
    .elementHandle();
  const navigation = await page
    .getByRole("navigation", { name: "主导航" })
    .elementHandle();
  const documents: string[] = [];
  page.on("request", (request) => {
    if (request.isNavigationRequest() && request.resourceType() === "document")
      documents.push(request.url());
  });
  await warning.getByRole("link", { name: "查看详情", exact: true }).click();
  await expect(page).toHaveURL(detailsPath);
  await expect(
    page.getByRole("heading", { name: "调节详情", exact: true }),
  ).toBeVisible();
  const request = page.getByTestId("rate-trigger-request");
  for (const value of [
    requestKey,
    "backup-archive",
    "source-archive",
    "ServiceUnavailable",
    "req-503-details-001",
    "extended-request-details-001",
    "1048",
  ])
    await expect(request).toContainText(value);
  await expect(request).toContainText("503");
  await expect(page.getByText("触发条件", { exact: true })).toBeVisible();
  await expect(page.getByText("调整记录", { exact: true })).toBeVisible();
  expect(state.calls[0].searchParams.get("workerIp")).toBe(nodeIp);
  expect(state.calls[0].searchParams.get("eventId")).toBe(eventId);
  expect(await toolbar!.evaluate((element) => element.isConnected)).toBe(true);
  expect(await navigation!.evaluate((element) => element.isConnected)).toBe(
    true,
  );
  expect(documents).toEqual([]);
  await expect(page.locator("iframe")).toHaveCount(0);
  await page.getByRole("link", { name: "返回执行节点", exact: true }).click();
  await expect(page).toHaveURL("/nodes");
});

test("adjustment history appends the next page and refreshes from the first page", async ({
  page,
}) => {
  const state = await setup(page);
  state.response.nextCursor = "cursor:2/+";
  state.response.done = false;
  state.nextPage = {
    ...detailResponse(),
    events: [change(3, 1000, "RECOVERED")],
    incidentActive: false,
  };
  await page.goto(detailsPath);
  const records = page.getByTestId("rate-adjustment-events");
  await expect(records).toContainText("自动降速");
  await expect(records).toContainText("逐步恢复");
  await expect(records).not.toContainText("已恢复目标");
  await page.getByRole("button", { name: "加载更多", exact: true }).click();
  await expect(records).toContainText("已恢复目标");
  await expect(records).toContainText("自动降速");
  const entries = records.getByRole("listitem");
  await expect(entries).toHaveCount(3);
  await expect(entries.nth(0)).toContainText("自动降速");
  await expect(entries.nth(1)).toContainText("逐步恢复");
  await expect(entries.nth(2)).toContainText("已恢复目标");
  const nextPageRequest = state.calls.find((url) =>
    url.searchParams.has("cursor"),
  );
  expect(nextPageRequest?.searchParams.get("cursor")).toBe("cursor:2/+");
  await expect(
    page.getByRole("button", { name: "加载更多", exact: true }),
  ).toHaveCount(0);
  const beforeRefresh = state.calls.length;
  await page.getByRole("button", { name: "刷新调节详情", exact: true }).click();
  await expect.poll(() => state.calls.length).toBeGreaterThan(beforeRefresh);
  expect(state.calls.at(-1)?.searchParams.has("cursor")).toBe(false);
  await expect(records).not.toContainText("已恢复目标");
});

test("old records explicitly report missing request details", async ({
  page,
}) => {
  const state = await setup(page);
  state.response.event = { ...state.response.event, triggerRequest: null };
  state.response.triggerEvent = null;
  state.response.events = [state.response.event];
  await page.goto(detailsPath);
  await expect(page.getByText(/^未记录触发请求详情。?$/)).toBeVisible();
  await expect(
    page.getByText("req-503-details-001", { exact: true }),
  ).toHaveCount(0);
  await expect(page.getByTestId("rate-adjustment-events")).toContainText(
    "自动降速",
  );
});

test("a restarted node is distinguished from natural recovery", async ({
  page,
}) => {
  const state = await setup(page, false);
  state.response.processRestarted = true;
  state.response.incidentActive = false;
  state.response.events = [state.response.event];
  await page.goto(detailsPath);
  await expect(
    page.getByRole("alert").filter({ hasText: /重启/ }),
  ).toBeVisible();
  await expect(page.getByTestId("rate-adjustment-events")).not.toContainText(
    "已恢复目标",
  );
  await expect(page.getByText(/已自然恢复|已恢复到设定上限/)).toHaveCount(0);
});

test("a failed details refresh preserves old records with an explicit error", async ({
  page,
}) => {
  const state = await setup(page);
  await page.goto(detailsPath);
  await expect(page.getByTestId("rate-adjustment-events")).toContainText(
    "自动降速",
  );
  state.error = "该节点暂时不可达，调节详情未更新";
  await page.getByRole("button", { name: "刷新调节详情", exact: true }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: state.error }),
  ).toBeVisible();
  await expect(page.getByTestId("rate-adjustment-events")).toContainText(
    "自动降速",
  );
  await expect(page.getByText(/暂无调整记录|已恢复到设定上限/)).toHaveCount(0);
});

test("initial detail failure is not presented as an empty successful history", async ({
  page,
}) => {
  const state = await setup(page);
  state.error = "未找到此调节记录";
  await page.goto(detailsPath);
  await expect(
    page.getByRole("alert").filter({ hasText: state.error }),
  ).toBeVisible();
  await expect(page.getByText(/暂无调整记录|已恢复到设定上限/)).toHaveCount(0);
  await expect(page.getByTestId("rate-trigger-request")).toHaveCount(0);
});

test("request details remain readable on a narrow phone without document overflow", async ({
  page,
}, testInfo) => {
  await page.setViewportSize({ width: 360, height: 800 });
  const state = await setup(page);
  state.response.triggerEvent.triggerRequest.key = `${requestKey}/${"a".repeat(300)}.bin`;
  await page.goto(detailsPath);
  await expect(
    page.getByRole("heading", { name: "调节详情", exact: true }),
  ).toBeVisible();
  await expect(page.getByTestId("rate-trigger-request")).toContainText(
    requestKey,
  );
  await expect
    .poll(() =>
      page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    )
    .toBe(true);
  await page.screenshot({
    path: testInfo.outputPath("rate-adjustment-details-mobile-360.png"),
    fullPage: true,
  });
});

test("request details render in the desktop dark theme", async ({
  page,
}, testInfo) => {
  await page.setViewportSize({ width: 1440, height: 1000 });
  await setup(page);
  await page.goto(detailsPath);
  await expect(page.getByTestId("rate-trigger-request")).toContainText(
    requestKey,
  );
  await page.getByRole("button", { name: "切换主题", exact: true }).click();
  await expect(page.locator("html")).toHaveCSS("color-scheme", "dark");
  await expect
    .poll(() =>
      page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    )
    .toBe(true);
  await page.screenshot({
    path: testInfo.outputPath("rate-adjustment-details-desktop-dark.png"),
    fullPage: true,
  });
});

test("partial persisted history is not presented as complete", async ({
  page,
}) => {
  const state = await setup(page);
  state.response.historyComplete = false;
  state.response.triggerEvent = null;
  state.response.event.triggerRequest = null;
  await page.goto(detailsPath);
  await expect(
    page.getByRole("alert").filter({ hasText: "本轮部分记录未保存" }),
  ).toBeVisible();
  await expect(
    page.getByText("未记录触发请求详情。", { exact: true }),
  ).toBeVisible();
});
