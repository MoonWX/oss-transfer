import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

const nodeIp = "10.0.1.12";
const processId = "12345678-1234-1234-1234-123456789abc";
const eventId = `${processId}:1`;
const path = `/nodes/adjustments?${new URLSearchParams({ workerIp: nodeIp, eventId })}`;
const cursor = "page:200/+";
const startedAt = 1790071200000;
function event(sequence: number, recovered = false) {
  return {
    id: `${processId}:${sequence}`,
    processId,
    sequence,
    incidentId: "one-incident",
    type: "put",
    manual: false,
    previousValue: sequence === 1 ? 1000 : 700,
    value: recovered ? 1000 : 840,
    manualTarget: 1000,
    reasonCode:
      sequence === 1 ? "OSS_503" : recovered ? "RECOVERED" : "RECOVERY_STEP",
    timestamp: startedAt + sequence * 1000,
    consecutive503Count: 20,
    consecutive503Threshold: 20,
    triggerRequest:
      sequence === 1
        ? {
            operation: "PUT_OBJECT",
            method: "PUT",
            statusCode: 503,
            requestId: "pagination-trigger",
          }
        : null,
  };
}
const headEvents = Array.from({ length: 200 }, (_, index) => event(index + 1));
async function setup(page: Page, refreshSeconds = 3600) {
  await mockApi(page);
  await page.addInitScript(
    (seconds) =>
      localStorage.setItem("oss-transfer-refresh-seconds", String(seconds)),
    refreshSeconds,
  );
  const state = {
    recovered: false,
    fail: false,
    hold: false,
    headCalls: 0,
    tailCalls: 0,
    heldCalls: 0,
    release: () => {},
  };
  let release: (() => void) | undefined;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  state.release = () => release?.();
  const response = (tail: boolean) => ({
    success: true,
    workerIp: nodeIp,
    event: headEvents[0],
    incidentId: "one-incident",
    triggerEvent: headEvents[0],
    events: tail
      ? [event(201), event(202), ...(state.recovered ? [event(203, true)] : [])]
      : headEvents,
    nextCursor: tail ? null : cursor,
    done: tail,
    incidentActive: !state.recovered,
    processRestarted: false,
  });
  await page.route("**/api/master/worker-status", (route) =>
    route.fulfill({
      json: { success: true, workers: { [nodeIp]: { success: true } } },
    }),
  );
  await page.route("**/api/master/rate-limit/adjustment?*", async (route) => {
    const tail = new URL(route.request().url()).searchParams.has("cursor");
    if (tail) {
      expect(new URL(route.request().url()).searchParams.get("cursor")).toBe(
        cursor,
      );
      state.tailCalls++;
    } else state.headCalls++;
    if (state.hold) {
      state.heldCalls++;
      await gate;
    }
    if (state.fail)
      return route.fulfill({
        status: 503,
        json: { success: false, message: "节点暂时不可达，详情未更新" },
      });
    return route.fulfill({ json: response(tail) });
  });
  await page.goto(path);
  await expect(
    page.getByTestId("rate-adjustment-events").getByRole("listitem"),
  ).toHaveCount(200);
  return state;
}

test("automatic polling refreshes a loaded final page when recovery is appended", async ({
  page,
}) => {
  const state = await setup(page, 1);
  const records = page.getByTestId("rate-adjustment-events");
  await page.getByRole("button", { name: "加载更多", exact: true }).click();
  await expect(records.getByRole("listitem")).toHaveCount(202);
  await expect(
    page.getByRole("button", { name: "加载更多", exact: true }),
  ).toHaveCount(0);
  await expect(records).not.toContainText("已恢复目标");
  const beforeHead = state.headCalls;
  const beforeTail = state.tailCalls;
  state.recovered = true;
  // The first 200 immutable events never change. Polling must re-read the loaded tail.
  await expect.poll(() => state.headCalls).toBeGreaterThan(beforeHead);
  await expect.poll(() => state.tailCalls).toBeGreaterThan(beforeTail);
  await expect(records.getByRole("listitem")).toHaveCount(203);
  await expect(records.getByRole("listitem").last()).toContainText(
    "已恢复目标",
  );
  await expect(records.getByRole("listitem").first()).toContainText("自动降速");
});

test("a failed manual refresh keeps previously loaded history pages", async ({
  page,
}) => {
  const state = await setup(page);
  state.recovered = true;
  await page.getByRole("button", { name: "加载更多", exact: true }).click();
  const records = page.getByTestId("rate-adjustment-events");
  await expect(records.getByRole("listitem")).toHaveCount(203);
  await expect(records.getByRole("listitem").last()).toContainText(
    "已恢复目标",
  );
  state.fail = true;
  await page.getByRole("button", { name: "刷新调节详情", exact: true }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "节点暂时不可达，详情未更新" }),
  ).toBeVisible();
  await expect(records.getByRole("listitem")).toHaveCount(203);
  await expect(records.getByRole("listitem").last()).toContainText(
    "已恢复目标",
  );
});

test("an in-flight detail refresh describes the last observation rather than a live state", async ({
  page,
}) => {
  const state = await setup(page);
  state.hold = true;
  await page.getByRole("button", { name: "刷新调节详情", exact: true }).click();
  await expect.poll(() => state.heldCalls).toBe(1);
  await expect(
    page.getByRole("button", { name: "刷新调节详情", exact: true }),
  ).toBeDisabled();
  const snapshot = page.getByRole("alert").filter({ hasText: /最近读取时/ });
  await expect(snapshot).toBeVisible();
  await expect(snapshot).not.toContainText("当前尚未恢复");
  await expect(
    page.getByTestId("rate-adjustment-events").getByRole("listitem"),
  ).toHaveCount(200);
  state.hold = false;
  state.release();
  await expect(
    page.getByRole("button", { name: "刷新调节详情", exact: true }),
  ).toBeEnabled();
});
