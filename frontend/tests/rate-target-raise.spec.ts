import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

const nodeIp = "10.0.1.12";
const processId = "raise-process";

const step = (sequence: number, previousValue: number, value: number) => ({
  id: `${processId}:${sequence}`,
  processId,
  sequence,
  type: "put",
  manual: false,
  previousValue,
  value,
  manualTarget: 3000,
  reasonCode: "RAISE_STEP",
  timestamp: Date.now(),
  intervalMs: 10000,
  increaseFactor: 1.2,
});

// After putQps is raised in service.json and the Agent restarts, the running
// limit climbs to the new target in steps. Nothing throttled the node, so it
// is shown as rising, not as reduced by storage throttling.
function raisingWorker(limit: number, events: ReturnType<typeof step>[]) {
  return {
    success: true,
    rateLimit: {
      put: {
        limitQps: limit,
        manualLimitQps: 3000,
        configuredLimitQps: 3000,
        currentQps: 20,
        limitSource: "CONFIG",
        state: "RAISING",
        lastAdjustment: events.at(-1),
      },
      head: {
        limitQps: 2800,
        manualLimitQps: 2800,
        configuredLimitQps: 2800,
        currentQps: 2,
        limitSource: "CONFIG",
        state: "NORMAL",
      },
      autoAdjust: {
        processId,
        observedAt: Date.now(),
        sequence: events.at(-1)?.sequence ?? 0,
        events,
        intervalMs: 10000,
        increaseFactor: 1.2,
        reduceFactor: 0.9,
        minQps: 200,
      },
    },
  };
}

async function setup(page: Page) {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "3600"),
  );
  let current = raisingWorker(1400, []);
  await page.route("**/api/master/worker-status", (route) =>
    route.fulfill({
      json: { success: true, workers: { [nodeIp]: current } },
    }),
  );
  return (worker: ReturnType<typeof raisingWorker>) => {
    current = worker;
  };
}

test("a limit climbing to a raised target is not reported as throttling", async ({
  page,
}) => {
  const update = await setup(page);
  await page.goto("/nodes");
  const card = page.getByTestId(`rate-${nodeIp}-put`);
  await expect(card).toContainText("按新目标逐步提升");
  await expect(card).not.toContainText("自动降速");
  await expect(page.getByTestId(`rate-notice-${nodeIp}`)).toHaveCount(0);
  // A step after the first snapshot is a new event, which must not raise a
  // throttling notice.
  update(raisingWorker(1680, [step(1, 1400, 1680)]));
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(card).toContainText("1,680 次/秒");
  await expect(card).toContainText("按新目标逐步提升");
  await expect(page.locator(".MuiSnackbar-root")).toHaveCount(0);
  await expect(page.getByTestId(`rate-notice-${nodeIp}`)).toHaveCount(0);
  // Nor does any other page carry a throttling banner.
  await page
    .getByRole("navigation", { name: "主导航" })
    .getByRole("link", { name: "迁移任务", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "迁移任务", exact: true }),
  ).toBeVisible();
  await expect(page.getByText(/可能遭遇存储服务限速/)).toHaveCount(0);
  await expect(page.getByText(/正在恢复请求上限/)).toHaveCount(0);
});

test("the details of a step to a raised target name its reason, not a trigger request", async ({
  page,
}) => {
  await mockApi(page);
  const event = step(1, 1400, 1680);
  await page.route("**/api/master/rate-limit/adjustment?*", (route) =>
    route.fulfill({
      json: {
        success: true,
        workerIp: nodeIp,
        event,
        triggerEvent: null,
        events: [event],
        done: true,
        processRestarted: false,
        incidentActive: false,
        historyComplete: true,
      },
    }),
  );
  await page.goto(
    `/nodes/adjustments?workerIp=${nodeIp}&eventId=${encodeURIComponent(event.id)}`,
  );
  await expect(page.getByTestId("rate-raise-reason")).toContainText(
    "目标已调高（如修改 service.json 后重启），运行上限逐步提升；仍低于目标。未出现限流。",
  );
  await expect(page.getByText("触发本次降速的请求")).toHaveCount(0);
  await expect(page.getByText("未记录触发请求详情。")).toHaveCount(0);
  await expect(page.getByTestId("rate-adjustment-events")).toContainText(
    "按新目标逐步提升",
  );
});

// The new limit is the lower of the running limit and the peak one-second
// rate, times the factor. The page shows that rule and the change it made,
// not a "10% reduction" that contradicts 1,400 → 270.
test("a reduction shows the rule it used and how far the limit fell", async ({
  page,
}) => {
  await mockApi(page);
  const trigger = {
    id: `${processId}:2`,
    processId,
    sequence: 2,
    incidentId: `${processId}-2`,
    type: "put",
    manual: false,
    previousValue: 1400,
    value: 270,
    manualTarget: 1400,
    reasonCode: "OSS_503",
    timestamp: Date.now(),
    throttleCount: 12,
    throttleResponses: 300,
    throttleThreshold: 5,
    throttleRatio: 0.01,
    throttleWindowMs: 5000,
    measuredQps: 300,
    factor: 0.9,
    minQps: 200,
  };
  await page.route("**/api/master/rate-limit/adjustment?*", (route) =>
    route.fulfill({
      json: {
        success: true,
        workerIp: nodeIp,
        event: trigger,
        incidentId: trigger.incidentId,
        triggerEvent: trigger,
        events: [trigger],
        done: true,
        processRestarted: false,
        incidentActive: true,
        historyComplete: true,
      },
    }),
  );
  await page.goto(
    `/nodes/adjustments?workerIp=${nodeIp}&eventId=${encodeURIComponent(trigger.id)}`,
  );
  for (const text of [
    "降速前单秒峰值",
    "300 次/秒",
    "降为原运行上限与降速前单秒峰值中较低者的 90%",
    "PUT 1,400 QPS → 270 QPS（下降 80.7%）",
  ])
    await expect(page.getByText(text, { exact: true })).toBeVisible();
  await expect(page.getByText("降速幅度")).toHaveCount(0);
  await expect(page.getByText("降速前实测速率")).toHaveCount(0);
});
