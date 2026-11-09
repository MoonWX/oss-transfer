import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

const serverMessage = "Some Agents did not confirm the rate update";
const nodeResult = "agent (10.0.1.13)：context deadline exceeded";
// The Master marks Agents that did not confirm a rate change with a stable
// code and keeps its English message for API and CLI clients.
const unconfirmed = {
  success: false,
  code: "RATE_UPDATE_UNCONFIRMED",
  message: serverMessage,
  successCount: 1,
  failedCount: 1,
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
  const alert = dialog.getByRole("alert").filter({ hasText: nodeResult });
  await expect(alert).toContainText(
    "1 个执行节点未确认限速设置，请核对节点状态后重试。",
  );
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
      "1 node did not confirm the new limit. Check node status and try again.",
  },
  {
    locale: "ja-JP",
    labels: {
      open: "制限を変更",
      input: "リクエスト上限（回/秒）",
      apply: "設定を適用",
    },
    message:
      "1 台の実行ノードで新しい制限値を確認できませんでした。ノードの状態を確認してから再試行してください。",
  },
]) {
  test.describe(`unconfirmed rate update in ${language.locale}`, () => {
    test.use({ locale: language.locale });
    test("translates the outcome and keeps node results verbatim", async ({
      page,
    }) => {
      const dialog = await applyPutLimit(page, unconfirmed, language.labels);
      await expect(
        dialog.getByRole("alert").filter({ hasText: nodeResult }),
      ).toContainText(language.message);
      await expect(dialog).not.toContainText(serverMessage);
    });
  });
}
