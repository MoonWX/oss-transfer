import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

const serverMessage = "Some Agents did not confirm the rate update";
// Known runtime reasons such as a timeout are translated; others stay as the
// Agent reported them.
const timedOut = {
  "zh-CN": "agent (10.0.1.13)：请求超时",
  "en-US": "agent (10.0.1.13)：The request timed out",
  "ja-JP": "agent (10.0.1.13)：リクエストがタイムアウトしました",
};
const unknownReason = "standby (10.0.1.14)：node controller is unavailable";
// The Master marks Agents that did not confirm a rate change with a stable
// code and keeps its English message for API and CLI clients.
const unconfirmed = {
  success: false,
  code: "RATE_UPDATE_UNCONFIRMED",
  message: serverMessage,
  successCount: 1,
  failedCount: 2,
  results: [
    {
      workerIp: "10.0.1.12",
      success: true,
      displayName: "master",
      displayAddress: "10.0.1.12",
      displayLabel: "master (10.0.1.12)",
      identityOnly: false,
      rateLimit: { put: { limitQps: 100 } },
    },
    {
      workerIp: "10.0.1.13",
      success: false,
      message: "context deadline exceeded",
      displayName: "agent",
      displayAddress: "10.0.1.13",
      displayLabel: "agent (10.0.1.13)",
      identityOnly: false,
    },
    {
      workerIp: "10.0.1.14",
      success: false,
      message: "node controller is unavailable",
      displayName: "standby",
      displayAddress: "10.0.1.14",
      displayLabel: "standby (10.0.1.14)",
      identityOnly: false,
    },
  ],
};

async function applyPutLimit(
  page: Page,
  response: object,
  labels: { open: string; input: string; apply: string },
) {
  await mockApi(page);
  const bodies: unknown[] = [];
  await page.route("**/api/master/rate-limit/put", (route) => {
    bodies.push(route.request().postDataJSON());
    return route.fulfill({ json: response });
  });
  await page.goto("/nodes");
  await page.getByRole("button", { name: labels.open, exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("spinbutton", { name: labels.input }).fill("100");
  await dialog.getByRole("button", { name: labels.apply, exact: true }).click();
  await expect.poll(() => bodies).toEqual([{ qps: 100 }]);
  return dialog;
}

const chinese = {
  open: "调整限速",
  input: "请求上限（次/秒）",
  apply: "应用设置",
};

test("unconfirmed Agents are reported in the console language with each node's result", async ({
  page,
}) => {
  const dialog = await applyPutLimit(page, unconfirmed, chinese);
  const alert = dialog
    .getByRole("alert")
    .filter({ hasText: timedOut["zh-CN"] });
  await expect(alert).toContainText(
    "2 个执行节点未确认限速设置，请核对节点状态后重试。",
  );
  await expect(alert).toContainText(unknownReason);
  await expect(alert).not.toContainText("context deadline exceeded");
  // Only the node that did not confirm is listed.
  await expect(alert).not.toContainText("master (10.0.1.12)");
  await expect(dialog).not.toContainText(serverMessage);
});

test("a response without the code keeps the server message", async ({
  page,
}) => {
  // Older Masters report the same outcome without a code.
  const { code: _code, ...legacy } = unconfirmed;
  const dialog = await applyPutLimit(page, legacy, chinese);
  await expect(
    dialog.getByRole("alert").filter({ hasText: serverMessage }),
  ).toHaveText(serverMessage);
});

for (const language of [
  {
    locale: "en-US",
    labels: {
      open: "Adjust limits",
      input: "Request limit (requests/s)",
      apply: "Apply settings",
    },
    message:
      "2 nodes did not confirm the new limit. Check node status and try again.",
  },
  {
    locale: "ja-JP",
    labels: {
      open: "制限を変更",
      input: "リクエスト上限（回/秒）",
      apply: "設定を適用",
    },
    message:
      "2 台の実行ノードで新しい制限値を確認できませんでした。ノードの状態を確認してから再試行してください。",
  },
]) {
  test.describe(`unconfirmed rate update in ${language.locale}`, () => {
    test.use({ locale: language.locale });
    test("translates the outcome and known node reasons", async ({ page }) => {
      const dialog = await applyPutLimit(page, unconfirmed, language.labels);
      const alert = dialog.getByRole("alert").filter({
        hasText: timedOut[language.locale as keyof typeof timedOut],
      });
      await expect(alert).toContainText(language.message);
      await expect(alert).toContainText(unknownReason);
      await expect(dialog).not.toContainText(serverMessage);
    });
  });
}

// Agents before local storage limits reject them; retrying cannot help until
// they are upgraded, so they are named as too old instead of "unconfirmed".
const localLimitOutcome = (results: object[]) => ({
  success: false,
  code: "RATE_UPDATE_UNCONFIRMED",
  message: serverMessage,
  successCount: 1,
  failedCount: results.length - 1,
  results,
});
const confirmedNode = unconfirmed.results[0];
const tooOldNode = {
  ...unconfirmed.results[1],
  code: "RATE_TYPE_UNSUPPORTED",
  message: "This node's version does not support this limit; upgrade it first",
};

async function applyLocalBandwidth(page: Page, response: object) {
  await mockApi(page);
  await page.route("**/api/master/rate-limit/localBandwidth", (route) =>
    route.fulfill({ json: response }),
  );
  await page.goto("/nodes");
  await page.getByRole("button", { name: "调整限速", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("combobox", { name: "限制类型" }).click();
  await page.getByRole("option", { name: "本地读写带宽", exact: true }).click();
  await dialog
    .getByRole("spinbutton", { name: "带宽上限（Mbps）" })
    .fill("800");
  await dialog.getByRole("button", { name: "应用设置", exact: true }).click();
  return dialog.getByRole("alert").filter({ hasText: "agent (10.0.1.13)" });
}

test("nodes too old for a local storage limit are told to upgrade, not to retry", async ({
  page,
}) => {
  const alert = await applyLocalBandwidth(
    page,
    localLimitOutcome([confirmedNode, tooOldNode]),
  );
  await expect(alert).toContainText(
    "1 个执行节点的版本不支持“本地读写带宽”限速，请先升级。",
  );
  await expect(alert).toContainText(
    "agent (10.0.1.13)：此节点版本不支持“本地读写带宽”限速，请先升级。",
  );
  await expect(alert).not.toContainText("未确认");
  await expect(alert).not.toContainText("重试");
});

test("a node too old for the limit and an unconfirmed node are reported apart", async ({
  page,
}) => {
  const alert = await applyLocalBandwidth(
    page,
    localLimitOutcome([confirmedNode, tooOldNode, unconfirmed.results[2]]),
  );
  await expect(alert).toContainText(
    "1 个执行节点未确认限速设置，请核对节点状态后重试。",
  );
  await expect(alert).toContainText(
    "1 个执行节点的版本不支持“本地读写带宽”限速，请先升级。",
  );
  await expect(alert).toContainText(
    "agent (10.0.1.13)：此节点版本不支持“本地读写带宽”限速，请先升级。",
  );
  await expect(alert).toContainText(unknownReason);
});

test("local storage limits are not offered for a node too old for them", async ({
  page,
}) => {
  const requests = await mockApi(page);
  const rateLimit = {
    list: { limitQps: 300 },
    put: { limitQps: 500 },
    head: { limitQps: 500 },
    bandwidth: { limitBps: 1000000000 },
  };
  // The master node reports local storage limits; the agent predates them.
  await page.route("**/api/master/worker-status", (route) =>
    route.fulfill({
      json: {
        success: true,
        workers: {
          "10.0.1.12": {
            success: true,
            rateLimit: {
              ...rateLimit,
              localOps: { limitQps: 0 },
              localBandwidth: { limitBps: 0 },
            },
          },
          "10.0.1.13": { success: true, rateLimit },
        },
      },
    }),
  );
  await page.goto("/nodes");
  await page.getByRole("button", { name: "调整限速", exact: true }).click();
  const dialog = page.getByRole("dialog");
  const type = dialog.getByRole("combobox", { name: "限制类型" });
  const scope = dialog.getByRole("combobox", { name: "应用范围" });
  const apply = dialog.getByRole("button", { name: "应用设置", exact: true });
  await type.click();
  await page.getByRole("option", { name: "本地读写带宽", exact: true }).click();
  // All nodes: the limit is applied where it exists, and the rest are named.
  await expect(
    dialog.getByText(
      "1 个执行节点的版本不支持“本地读写带宽”限速，不会应用此设置。",
      { exact: true },
    ),
  ).toBeVisible();
  await expect(apply).toBeEnabled();
  // The older node alone cannot take the limit at all.
  await scope.click();
  await page
    .getByRole("option", { name: "agent (10.0.1.13)", exact: true })
    .click();
  await expect(
    dialog.getByText("此节点版本不支持“本地读写带宽”限速，请先升级。", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(apply).toBeDisabled();
  await type.click();
  for (const name of ["本地读写带宽", "本地文件操作"])
    await expect(
      page.getByRole("option", { name, exact: true }),
    ).toHaveAttribute("aria-disabled", "true");
  await page.getByRole("option", { name: "PUT 请求", exact: true }).click();
  await expect(apply).toBeEnabled();
  await expect(dialog.getByText(/不支持/)).toHaveCount(0);
  // The current node is offered both.
  await scope.click();
  await page
    .getByRole("option", { name: "master (10.0.1.12)", exact: true })
    .click();
  await type.click();
  await expect(
    page.getByRole("option", { name: "本地文件操作", exact: true }),
  ).not.toHaveAttribute("aria-disabled", "true");
  await page.keyboard.press("Escape");
  expect(
    requests.filter(
      ({ path, method }) => method === "POST" && path.includes("/rate-limit/"),
    ),
  ).toHaveLength(0);
});
