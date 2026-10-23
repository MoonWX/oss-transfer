import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

const nodeIp = "10.0.1.12";
function workerState(limit = 729, state = "REDUCED", sequence = 1) {
  const timestamp = Date.now();
  const event = {
    id: `test-process:${sequence}`,
    processId: "test-process",
    sequence,
    type: "put",
    manual: false,
    previousValue: sequence === 1 ? 810 : sequence === 2 ? 729 : 874.8,
    value: limit,
    manualTarget: 1000,
    reasonCode:
      state === "RECOVERED"
        ? "RECOVERED"
        : state === "RECOVERING"
          ? "RECOVERY_STEP"
          : "OSS_503",
    timestamp,
    consecutive503Count: 23,
    consecutive503Threshold: 20,
  };
  return {
    success: true,
    taskStats: { runningTasks: 1, pendingTasks: 0, completedTasks: 1 },
    rateLimit: {
      list: {
        limitQps: 100,
        manualLimitQps: 100,
        currentQps: 8,
        state: "NORMAL",
        limitSource: "CONFIG",
      },
      put: {
        limitQps: limit,
        manualLimitQps: 1000,
        currentQps: 12,
        state,
        limitSource: "PERSISTED_MANUAL",
        lastAdjustment: event,
      },
      head: {
        limitQps: 2000,
        manualLimitQps: 2000,
        currentQps: 6,
        state: "NORMAL",
        limitSource: "CONFIG",
      },
      bandwidth: {
        limitBps: 1000000000,
        manualLimitBps: 1000000000,
        currentBps: 10000,
        state: "NORMAL",
        limitSource: "CONFIG",
      },
      autoAdjust: {
        processId: "test-process",
        // Node clock skew must not make a freshly received snapshot stale.
        observedAt: timestamp - 300000,
        intervalMs: 10000,
        increaseFactor: 1.2,
        reduceFactor: 0.9,
        minQps: 200,
        consecutive503Count: 23,
        consecutive503Threshold: 20,
        last503At: timestamp,
        nextRecoveryAt: timestamp + 10000,
        sequence,
        events: [event],
      },
    },
  };
}
async function setup(page: Page) {
  await mockApi(page);
  const state: { worker: any; error: boolean } = {
    worker: workerState(),
    error: false,
  };
  await page.route("**/api/master/worker-status", async (route) => {
    if (state.error)
      return route.fulfill({
        status: 503,
        json: { success: false, message: "节点状态读取失败" },
      });
    await route.fulfill({
      json: { success: true, workers: { [nodeIp]: state.worker } },
    });
  });
  await page.route("**/api/master/local-nodes", async (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          nodes: [
            {
              executionNodeIp: nodeIp,
              reachable: true,
              localTransferSupported: true,
            },
          ],
        },
      },
    }),
  );
  return state;
}

test("node links active adjustments to details and keeps recovery progress concise", async ({
  page,
}, testInfo) => {
  const state = await setup(page);
  await page.goto("/nodes");
  const put = page.getByTestId(`rate-${nodeIp}-put`);
  await expect(put.getByText("自动降速", { exact: true })).toBeVisible();
  await expect(put.getByText("已保存目标", { exact: true })).toBeVisible();
  await expect(put.getByText("1,000 次/秒", { exact: true })).toBeVisible();
  await expect(put.getByText("12 次/秒", { exact: true })).toBeVisible();
  await expect(put.getByRole("progressbar")).toHaveAttribute(
    "aria-valuenow",
    "73",
  );
  await expect(put.getByRole("progressbar")).toHaveAccessibleName(
    "PUT 请求运行上限占目标比例",
  );
  const notice = page.getByTestId(`rate-notice-${nodeIp}`);
  await expect(
    notice.getByText("节点可能遭遇限速，已自动调整运行上限。"),
  ).toBeVisible();
  await expect(notice.getByRole("link", { name: "查看详情" })).toHaveAttribute(
    "href",
    "/nodes/adjustments?workerIp=10.0.1.12&eventId=test-process%3A1",
  );
  await expect(page.getByText("自动调节依据", { exact: true })).toHaveCount(0);
  await expect(page.getByText(/实测速率按.*秒窗口统计/)).toHaveCount(0);
  await expect(put.getByText(/最近变化/)).toHaveCount(0);
  await put
    .getByRole("button", { name: `调整 master (${nodeIp}) 的 PUT 请求 限速` })
    .click();
  const dialog = page.getByRole("dialog");
  await expect(
    dialog.getByRole("spinbutton", { name: "请求上限（次/秒）" }),
  ).toHaveValue("1000");
  await dialog.getByRole("combobox", { name: "限制类型" }).click();
  await page.getByRole("option", { name: "HEAD 请求", exact: true }).click();
  await expect(
    dialog.getByRole("spinbutton", { name: "请求上限（次/秒）" }),
  ).toHaveValue("2000");
  await dialog.getByRole("button", { name: "取消", exact: true }).click();
  state.worker = workerState(874.8, "RECOVERING", 2);
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(
    put.getByText("逐步恢复", { exact: true }).first(),
  ).toBeVisible();
  await expect(put.getByRole("progressbar")).toHaveAttribute(
    "aria-valuenow",
    "87",
  );
  await expect(notice.getByRole("link", { name: "查看详情" })).toHaveAttribute(
    "href",
    "/nodes/adjustments?workerIp=10.0.1.12&eventId=test-process%3A2",
  );
  await put.screenshot({ path: testInfo.outputPath("rate-recovering.png") });
  state.worker = workerState(1000, "RECOVERED", 3);
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(put.getByText("已恢复目标", { exact: true })).toBeVisible();
  await expect(put.getByRole("progressbar")).toHaveAttribute(
    "aria-valuenow",
    "100",
  );
  await expect(notice).toHaveCount(0);
  await put.screenshot({ path: testInfo.outputPath("rate-recovered.png") });
});

test("failed refresh, paused polling and offline nodes do not claim recovery", async ({
  page,
}) => {
  const state = await setup(page);
  state.worker = workerState(1000, "RECOVERED", 3);
  await page.goto("/nodes");
  const put = page.getByTestId(`rate-${nodeIp}-put`);
  await expect(put.getByText("已恢复目标", { exact: true })).toBeVisible();
  state.error = true;
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(put.getByText("状态待确认", { exact: true })).toBeVisible();
  await expect(put.getByRole("progressbar")).toHaveCount(0);
  state.error = false;
  state.worker = workerState(874.8, "RECOVERING", 2);
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(
    put.getByText("逐步恢复", { exact: true }).first(),
  ).toBeVisible();
  await page.getByRole("combobox", { name: /^自动刷新/ }).click();
  await page.getByRole("option", { name: "暂停", exact: true }).click();
  await expect(put.getByText("状态待确认", { exact: true })).toBeVisible();
  await expect(
    page.getByText("自动刷新已暂停。以下状态为最近一次读取结果。"),
  ).toBeVisible();
  await expect(put.getByRole("progressbar")).toHaveCount(0);
  await expect(page.getByText(/下次恢复计划/)).toHaveCount(0);
  state.worker = { success: false, message: "节点离线" };
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(page.getByText("节点离线", { exact: true })).toBeVisible();
  await expect(put).toHaveCount(0);
});

test("legacy nodes and partial history do not invent targets or reasons", async ({
  page,
}) => {
  const state = await setup(page);
  state.worker = {
    success: true,
    rateLimit: { put: { limitQps: 729, currentQps: 12 } },
  };
  await page.route("**/api/master/rate-limit/history?*", async (route) =>
    route.fulfill({
      json: {
        success: true,
        partial: true,
        failedWorkers: ["10.0.1.13"],
        adjustments: [
          {
            workerIp: nodeIp,
            type: "put",
            value: 729,
            previousValue: null,
            manualTarget: null,
            reasonCode: null,
            manual: false,
            timestamp: Date.now(),
          },
          {
            id: "test-process:1",
            workerIp: nodeIp,
            type: "put",
            previousValue: 810,
            value: 729,
            manualTarget: 1000,
            reasonCode: "OSS_503",
            consecutive503Count: 23,
            consecutive503Threshold: 20,
            timestamp: Date.now() - 1000,
          },
        ],
      },
    }),
  );
  await page.goto("/nodes");
  const put = page.getByTestId(`rate-${nodeIp}-put`);
  await expect(put.getByText("—", { exact: true })).toBeVisible();
  await expect(page.getByTestId(`rate-notice-${nodeIp}`)).toHaveCount(0);
  await expect(put.getByRole("progressbar")).toHaveCount(0);
  await put
    .getByRole("button", { name: `调整 master (${nodeIp}) 的 PUT 请求 限速` })
    .click();
  await expect(page.getByRole("dialog").getByRole("spinbutton")).toHaveValue(
    "",
  );
  await page.getByRole("button", { name: "取消", exact: true }).click();
  await page.getByRole("tab", { name: "限速历史", exact: true }).click();
  await expect(
    page.getByText("限速历史不完整。未获取到节点 agent (10.0.1.13) 的记录。", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(
    page.getByRole("cell", { name: "历史记录未保存调整原因。", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("cell", { name: "— → 729 次/秒", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("cell", { name: "810 次/秒 → 729 次/秒", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("link", { name: "查看详情", exact: true }),
  ).toHaveAttribute(
    "href",
    "/nodes/adjustments?workerIp=10.0.1.12&eventId=test-process%3A1",
  );
  await expect(page.getByText("未记录详情", { exact: true })).toBeVisible();
});

test("node rate details fit a 360px mobile viewport", async ({
  page,
}, testInfo) => {
  await setup(page);
  await page.setViewportSize({ width: 360, height: 800 });
  await page.goto("/nodes");
  await expect(page.getByTestId(`rate-${nodeIp}-put`)).toBeVisible();
  await expect
    .poll(() =>
      page.evaluate(
        () => document.documentElement.scrollWidth - window.innerWidth,
      ),
    )
    .toBeLessThanOrEqual(1);
  await page.screenshot({
    path: testInfo.outputPath("node-rate-mobile.png"),
    fullPage: true,
  });
  await page.getByRole("tab", { name: "限速历史", exact: true }).click();
  await expect
    .poll(() =>
      page.evaluate(
        () => document.documentElement.scrollWidth - window.innerWidth,
      ),
    )
    .toBeLessThanOrEqual(1);
  await page.setViewportSize({ width: 1280, height: 1000 });
  await page.evaluate(() => localStorage.setItem("oss-transfer-theme", "dark"));
  await page.reload();
  await expect(page.getByTestId(`rate-${nodeIp}-put`)).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("node-rate-desktop-dark.png"),
    fullPage: true,
  });
});

test("rounded fractional targets do not imply automatic reduction", async ({
  page,
}) => {
  const state = await setup(page);
  state.worker = workerState(1.23, "NORMAL");
  state.worker.rateLimit.put.manualLimitQps = 1.234;
  state.worker.rateLimit.put.configuredLimitQps = 1.234;
  state.worker.rateLimit.put.lastAdjustment = {
    ...state.worker.rateLimit.put.lastAdjustment,
    manual: true,
    reasonCode: "MANUAL_SET",
  };
  state.worker.rateLimit.autoAdjust.events = [];
  await page.goto("/nodes");
  const put = page.getByTestId(`rate-${nodeIp}-put`);
  await expect(put.getByText("按目标运行", { exact: true })).toBeVisible();
  await expect(put.getByText("配置目标", { exact: true })).toBeVisible();
  await expect(put.getByText("已保存目标", { exact: true })).toHaveCount(0);
  await expect(put.getByText(/最近变化|用户修改上限/)).toHaveCount(0);
  await expect(page.getByTestId(`rate-notice-${nodeIp}`)).toHaveCount(0);
  await expect(page.getByText("自动调节依据", { exact: true })).toHaveCount(0);
  await expect(put.getByText("自动降速", { exact: true })).toHaveCount(0);
  await expect(put.getByRole("progressbar")).toHaveCount(0);
  await expect(page.getByText(/下次恢复计划/)).toHaveCount(0);
  await put
    .getByRole("button", { name: `调整 master (${nodeIp}) 的 PUT 请求 限速` })
    .click();
  await expect(page.getByRole("dialog").getByRole("spinbutton")).toHaveValue(
    "1.234",
  );
  await page.getByRole("button", { name: "取消", exact: true }).click();

  // A legacy response with values but no explicit state cannot prove an automatic cause.
  delete state.worker.rateLimit.put.state;
  state.worker.rateLimit.put.limitQps = 1;
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(put.getByText("按目标运行", { exact: true })).toHaveCount(0);
  await expect(put.getByText("自动降速", { exact: true })).toHaveCount(0);
  await expect(put.getByRole("progressbar")).toHaveCount(0);
  await expect(page.getByText(/下次恢复计划/)).toHaveCount(0);
});

test.describe("localized rate history timestamps", () => {
  test.use({ locale: "en-US", timezoneId: "UTC" });
  test("formats raw timestamps instead of reusing server-formatted text", async ({
    page,
  }) => {
    await mockApi(page);
    await page.route("**/api/master/rate-limit/history?*", (route) =>
      route.fulfill({
        json: {
          success: true,
          adjustments: [
            {
              workerIp: nodeIp,
              type: "put",
              value: 900,
              previousValue: 1000,
              timestamp: Date.parse("2026-09-24T01:02:03Z"),
              timestampFormatted: "2026年9月24日 旧格式",
              reasonCode: "MANUAL_SET",
              manual: true,
            },
          ],
        },
      }),
    );
    await page.goto("/nodes");
    await page.getByRole("tab", { name: "Limit history", exact: true }).click();
    await expect(
      page.getByRole("cell", { name: "9/24/2026, 01:02:03", exact: true }),
    ).toBeVisible();
    await expect(
      page.getByText("2026年9月24日 旧格式", { exact: true }),
    ).toHaveCount(0);
    await page.getByRole("button", { name: "Language", exact: true }).click();
    await page.getByRole("menuitem", { name: "日本語", exact: true }).click();
    await expect(
      page.getByRole("cell", { name: "2026/9/24 1:02:03", exact: true }),
    ).toBeVisible();
  });
});
