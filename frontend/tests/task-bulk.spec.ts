import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

function gate() {
  let release!: () => void;
  const promise = new Promise<void>((resolve) => {
    release = resolve;
  });
  return { promise, release };
}
const records = () =>
  Array.from({ length: 130 }, (_, index) => ({
    ...tasks[2],
    taskIndex: 130 - index,
    generation: 1,
    status: "PARTIAL_FAILED",
  }));
test.beforeEach(async ({ page }) => {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "0"),
  );
});
async function setup(page: Page, rows: ReturnType<typeof records>) {
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { tasks: rows } }),
  );
  await page.route(/\/api\/master\/tasks\/\d+$/, (route) => {
    const id = Number(route.request().url().split("/").pop());
    return route.fulfill({
      json: { data: rows.find((task) => task.taskIndex === id) },
    });
  });
  await page.goto("/tasks");
  await page
    .getByRole("checkbox", { name: "选择任务 130", exact: true })
    .check();
  await page
    .getByRole("button", { name: "选择全部 130 个结果", exact: true })
    .click();
  await expect(page.getByText("已选 130 项", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "重试失败项", exact: true }).click();
}
function accept(
  rows: ReturnType<typeof records>,
  ids: number[],
  fail?: number,
) {
  return {
    success: true,
    data: {
      accepted: ids.filter((id) => id !== fail).length,
      failed: ids.includes(fail ?? -1) ? 1 : 0,
      details: ids.map((id) => {
        const task = rows.find((row) => row.taskIndex === id)!;
        if (id === fail)
          return { taskId: id, success: false, message: "retry denied" };
        task.generation = 2;
        task.status = "RUNNING";
        return {
          taskId: id,
          success: true,
          accepted: true,
          generation: 2,
          status: "RESUMING",
        };
      }),
    },
  };
}

test("130 selected tasks show progress, reject duplicate clicks and keep only failed tasks selected", async ({
  page,
}) => {
  const rows = records(),
    second = gate();
  const requests: number[][] = [];
  await page.route("**/api/master/local-transfers/resume", async (route) => {
    const body = route.request().postDataJSON();
    requests.push(body.taskIds);
    expect(body.expectedGenerations).toEqual(
      Object.fromEntries(body.taskIds.map((id: number) => [id, 1])),
    );
    if (requests.length === 2) await second.promise;
    await route.fulfill({ json: accept(rows, body.taskIds, 1) });
  });
  await setup(page, rows);
  const dialog = page.getByRole("dialog");
  await dialog
    .getByRole("button", { name: "确认重试", exact: true })
    .evaluate((button: HTMLButtonElement) => {
      button.click();
      button.click();
    });
  await expect(dialog).toContainText("已处理 64 / 130");
  await expect(
    dialog.getByRole("button", { name: "提交中…", exact: true }),
  ).toBeDisabled();
  expect(requests).toHaveLength(2);
  second.release();
  await expect(dialog).toHaveCount(0);
  expect(requests.map((ids) => ids.length)).toEqual([64, 64, 2]);
  expect(requests.flat()).toEqual(
    Array.from({ length: 130 }, (_, i) => 130 - i),
  );
  await expect(page.getByText("已选 1 项", { exact: true })).toBeVisible();
  await expect(
    page.getByText("已受理 129，忽略 0，失败 1", { exact: true }),
  ).toBeVisible();
  await page.getByText("查看失败详情", { exact: true }).click();
  await expect(
    page.getByText("#1: retry denied", { exact: true }),
  ).toBeVisible();
});

test("a lost second response is not replayed and leaves the final batch unsubmitted and unlocked", async ({
  page,
}) => {
  const rows = records();
  const requests: number[][] = [];
  await page.route("**/api/master/local-transfers/resume", async (route) => {
    const ids = route.request().postDataJSON().taskIds;
    requests.push(ids);
    const response = accept(rows, ids);
    if (requests.length === 2) return route.abort("failed");
    return route.fulfill({ json: response });
  });
  await setup(page, rows);
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "确认重试", exact: true })
    .click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  expect(requests).toHaveLength(2);
  await expect(
    page.getByText("待确认 64，未提交 2", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText("已选 66 项", { exact: true })).toBeVisible();
  const locks = await page.evaluate(() =>
    JSON.parse(sessionStorage.getItem("oss-transfer-task-recovery") || "{}"),
  );
  expect(locks[1]).toBeUndefined();
  expect(locks[2]).toBeUndefined();
  expect(locks[66].phase).toBe("checking");
  await page.getByRole("button", { name: "刷新任务", exact: true }).click();
  expect(requests).toHaveLength(2);
});

test("missing task generation leaves every selected task unsubmitted without bypassing generation checks", async ({
  page,
}) => {
  const rows = records();
  rows[0].generation = 0;
  const requests: number[][] = [];
  await page.route("**/api/master/local-transfers/resume", (route) => {
    requests.push(route.request().postDataJSON().taskIds);
    return route.fulfill({ json: { success: true } });
  });
  await setup(page, rows);
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "确认重试", exact: true })
    .click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  expect(requests).toHaveLength(0);
  await expect(
    page.getByText("待确认 0，未提交 130", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText("已选 130 项", { exact: true })).toBeVisible();
  const locks = await page.evaluate(() =>
    JSON.parse(sessionStorage.getItem("oss-transfer-task-recovery") || "{}"),
  );
  expect(Object.keys(locks)).toHaveLength(0);
});
