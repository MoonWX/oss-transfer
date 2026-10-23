import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";
import type { Adjustment, Worker } from "../src/lib/rateStatus";

const event = (
  sequence: number,
  type: "put" | "head",
  reasonCode: string,
  previousValue: number,
  value: number,
): Adjustment => ({
  id: `process-a:${sequence}`,
  processId: "process-a",
  sequence,
  type,
  reasonCode,
  previousValue,
  value,
  manualTarget: 1000,
  manual: false,
  timestamp: Date.now(),
  consecutive503Count: 20,
  consecutive503Threshold: 20,
});
function worker(
  put: number,
  head: number,
  events: Adjustment[] = [],
  processId = "process-a",
): Worker {
  const rate = (type: "put" | "head", value: number) => {
    const last = events.filter((item) => item.type === type).at(-1);
    return {
      limitQps: value,
      currentQps: 20,
      manualLimitQps: 1000,
      limitSource: "PERSISTED_MANUAL" as const,
      state:
        value >= 1000
          ? last
            ? ("RECOVERED" as const)
            : ("NORMAL" as const)
          : last?.reasonCode === "OSS_503"
            ? ("REDUCED" as const)
            : ("RECOVERING" as const),
      lastAdjustment: last,
    };
  };
  return {
    success: true,
    rateLimit: {
      put: rate("put", put),
      head: rate("head", head),
      autoAdjust: {
        processId,
        observedAt: Date.now(),
        sequence: events.at(-1)?.sequence ?? 0,
        events,
        intervalMs: 10000,
        increaseFactor: 1.2,
        reduceFactor: 0.9,
        minQps: 200,
        consecutive503Count: 20,
        consecutive503Threshold: 20,
        last503At: Date.now(),
        nextRecoveryAt: Date.now() + 10000,
      },
    },
  };
}
async function setup(page: Page, initial = worker(1000, 1000)) {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "3600"),
  );
  let current = initial;
  let calls = 0;
  let failed = false;
  await page.route("**/api/master/worker-status", async (route) => {
    calls++;
    if (failed)
      return route.fulfill({
        status: 503,
        json: { success: false, message: "测试连接中断" },
      });
    return route.fulfill({
      json: {
        success: true,
        workers: { "10.0.1.12": current, "10.0.1.13": { success: true } },
      },
    });
  });
  await page.goto("/tasks");
  await expect(
    page.getByRole("button", { name: "刷新任务", exact: true }),
  ).toBeEnabled();
  await expect.poll(() => calls).toBe(1);
  return {
    update: (value: Worker) => {
      current = value;
    },
    calls: () => calls,
    fail: () => {
      failed = true;
    },
    refresh: async () => {
      await page.getByRole("button", { name: "刷新任务", exact: true }).click();
      await expect(
        page.getByRole("button", { name: "刷新任务", exact: true }),
      ).toBeEnabled();
    },
  };
}
const notice = (page: Page) => page.locator(".MuiSnackbar-root");

test("automatic reduction and recovery use brief notices without policy or request details", async ({
  page,
}) => {
  const state = await setup(page);
  const reductions = [
    event(1, "put", "OSS_503", 1000, 800),
    event(2, "head", "OSS_503", 1000, 900),
  ];
  state.update(worker(800, 900, reductions));
  await state.refresh();
  await expect(notice(page)).toContainText(
    "可能遭遇存储服务限速，已自动降低请求上限",
  );
  await expect(notice(page)).not.toContainText("QPS");
  await expect(notice(page)).not.toContainText("阈值");
  await expect(page.locator("main").getByRole("alert")).toContainText(
    "可能遭遇存储服务限速",
  );

  const partial = [
    ...reductions,
    event(3, "put", "RECOVERY_STEP", 800, 960),
    event(4, "head", "RECOVERED", 900, 1000),
  ];
  state.update(worker(960, 1000, partial));
  await state.refresh();
  await expect(notice(page)).toContainText("正在恢复请求上限");
  await expect(notice(page)).not.toContainText("已恢复到目标上限");
  await expect(
    page.locator("main").getByText(/正在恢复请求上限/),
  ).toBeVisible();

  state.update(
    worker(1000, 1000, [...partial, event(5, "put", "RECOVERED", 960, 1000)]),
  );
  await state.refresh();
  await expect(notice(page)).toContainText("已恢复到目标上限");
  await expect(page.locator("main").getByText(/正在恢复请求上限/)).toHaveCount(
    0,
  );
  await notice(page).getByRole("button", { name: "关闭" }).click();
  await state.refresh();
  await expect(notice(page)).toHaveCount(0);
});

test("first-load active state remains visible across pages without replaying old events or duplicating polling", async ({
  page,
}) => {
  const reductions = [
    event(1, "put", "OSS_503", 1000, 800),
    event(2, "head", "OSS_503", 1000, 900),
  ];
  const state = await setup(page, worker(800, 900, reductions));
  await expect(page.locator("main").getByRole("alert")).toContainText(
    "可能遭遇存储服务限速",
  );
  await expect(notice(page)).toHaveCount(0);
  for (const name of ["存储连接", "执行节点", "迁移任务"]) {
    await page
      .getByRole("navigation", { name: "主导航" })
      .getByRole("link", { name, exact: true })
      .click();
    await expect(
      page
        .locator("main")
        .getByText(/可能遭遇存储服务限速|可能遭遇限速/)
        .first(),
    ).toBeVisible();
    await expect(notice(page)).toHaveCount(0);
  }
  expect(state.calls()).toBe(1);
});

test("paused polling and failed refresh label retained state as outdated instead of live recovery", async ({
  page,
}) => {
  const state = await setup(
    page,
    worker(800, 900, [event(1, "put", "RECOVERY_STEP", 600, 800)]),
  );
  await page.getByRole("combobox", { name: /^自动刷新/ }).click();
  await page.getByRole("option", { name: "暂停", exact: true }).click();
  await expect(
    page
      .locator("main")
      .getByRole("alert")
      .filter({ hasText: "当前状态待更新" }),
  ).toBeVisible();
  await expect(page.locator("main").getByText(/正在恢复请求上限/)).toHaveCount(
    0,
  );
  await expect(page.locator("main").getByRole("alert")).not.toContainText(
    "恢复请求上限",
  );
  state.fail();
  await state.refresh();
  await expect(
    page
      .locator("main")
      .getByRole("alert")
      .filter({ hasText: "当前状态待更新" }),
  ).toBeVisible();
  await expect(notice(page)).toHaveCount(0);
});

test("event gaps retain a brief warning and a restarted process does not replay events", async ({
  page,
}) => {
  const state = await setup(page);
  state.update(
    worker(800, 900, [
      event(109, "put", "OSS_503", 900, 800),
      event(110, "head", "OSS_503", 1000, 900),
    ]),
  );
  await state.refresh();
  await expect(notice(page)).toContainText("可能遭遇存储服务限速");
  await expect(
    page.locator("main").getByRole("link", { name: "查看详情" }),
  ).toHaveAttribute("href", /eventId=process-a%3A110/);
  await notice(page).getByRole("button", { name: "关闭" }).click();
  state.update(worker(1000, 1000, [], "process-b"));
  await state.refresh();
  await expect(notice(page)).toHaveCount(0);
  await expect(
    page.locator("main").getByText(/可能遭遇存储服务限速/),
  ).toHaveCount(0);
});

test("a manual change after reduction is distinguished from the earlier automatic event", async ({
  page,
}) => {
  const state = await setup(page);
  const reductions = [event(1, "put", "OSS_503", 1000, 800)];
  const snapshot = worker(1000, 1000, reductions);
  const manual: Adjustment = {
    ...event(2, "put", "MANUAL_SET", 800, 1000),
    manual: true,
  };
  snapshot.rateLimit!.put = {
    ...snapshot.rateLimit!.put,
    state: "NORMAL",
    lastAdjustment: manual,
  };
  snapshot.rateLimit!.autoAdjust!.sequence = 2;
  snapshot.rateLimit!.autoAdjust!.events.push(manual);
  state.update(snapshot);
  await state.refresh();
  await expect(notice(page)).toContainText("上限已手动更新");
  await expect(
    page.locator("main").getByText(/可能遭遇存储服务限速/),
  ).toHaveCount(0);
});
