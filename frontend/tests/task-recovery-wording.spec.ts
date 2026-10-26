import { expect, test } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

const retryDescription = "重试失败或未完成的对象，已完成的对象不重复传输。";
const resumeDescription = "继续扫描和处理未完成的对象，保留已有传输进度。";

for (const entry of [
  {
    status: "PARTIAL_FAILED",
    label: "重试失败项",
    title: "重试 1 个任务的失败项？",
    confirm: "确认重试",
    description: retryDescription,
  },
  {
    status: "FAILED",
    label: "重试任务",
    title: "重试 1 个任务？",
    confirm: "确认重试",
    description: retryDescription,
  },
  {
    status: "STOPPED",
    label: "继续传输",
    title: "继续传输 1 个任务？",
    confirm: "确认继续传输",
    description: resumeDescription,
  },
]) {
  test(`${entry.status} task detail explains the correct recovery action`, async ({
    page,
  }) => {
    await mockApi(page);
    await page.addInitScript(() =>
      localStorage.setItem("oss-transfer-refresh-seconds", "0"),
    );
    let current = {
      ...tasks[2],
      taskIndex: 41,
      generation: 3,
      status: entry.status,
    };
    await page.route("**/api/master/status", (route) =>
      route.fulfill({ json: { tasks: [current] } }),
    );
    await page.route("**/api/master/tasks/41", (route) =>
      route.fulfill({ json: { data: current } }),
    );
    const sent: unknown[] = [];
    await page.route("**/api/master/local-transfers/resume", (route) => {
      sent.push(route.request().postDataJSON());
      current = { ...current, status: "RUNNING", generation: 4 };
      return route.fulfill({
        json: {
          success: true,
          data: {
            accepted: 1,
            ignored: 0,
            failed: 0,
            details: [
              {
                taskId: 41,
                status: "RUNNING",
                generation: 4,
                success: true,
                accepted: true,
              },
            ],
          },
        },
      });
    });
    await page.goto("/tasks?taskId=41");
    await page.getByRole("button", { name: entry.label, exact: true }).click();
    const dialog = page.getByRole("dialog", { name: entry.title, exact: true });
    await expect(dialog).toContainText(entry.description);
    await expect(dialog).toContainText("任务：#41");
    await dialog
      .getByRole("button", { name: entry.confirm, exact: true })
      .click();
    await expect(dialog).toBeHidden();
    await expect(
      page.getByRole("button", { name: "停止任务", exact: true }),
    ).toBeVisible();
    await expect(
      page.getByText(new RegExp(`^${entry.label}请求已提交`)),
    ).toBeVisible();
    expect(sent).toEqual([{ taskIds: [41], expectedGenerations: { "41": 3 } }]);
    expect(current.putObjectCount).toBe(tasks[2].putObjectCount);
  });
}

test("mixed failed and stopped selection explains both operations without changing request guards", async ({
  page,
}) => {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "0"),
  );
  const rows = [
    { ...tasks[2], taskIndex: 41, generation: 3, status: "PARTIAL_FAILED" },
    { ...tasks[2], taskIndex: 42, generation: 5, status: "STOPPED" },
  ];
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { tasks: rows } }),
  );
  await page.route(/\/api\/master\/tasks\/(41|42)$/, (route) => {
    const id = Number(route.request().url().split("/").at(-1));
    return route.fulfill({
      json: { data: rows.find((row) => row.taskIndex === id) },
    });
  });
  const sent: {
    taskIds: number[];
    expectedGenerations: Record<string, number>;
  }[] = [];
  await page.route("**/api/master/local-transfers/resume", (route) => {
    const body = route.request().postDataJSON();
    sent.push(body);
    const details = body.taskIds.map((id: number) => {
      const row = rows.find((item) => item.taskIndex === id)!;
      row.status = "RUNNING";
      row.generation++;
      return {
        taskId: id,
        status: row.status,
        generation: row.generation,
        success: true,
        accepted: true,
      };
    });
    return route.fulfill({
      json: {
        success: true,
        data: { accepted: 2, ignored: 0, failed: 0, details },
      },
    });
  });
  await page.goto("/tasks");
  await page
    .getByRole("checkbox", { name: "选择任务 41", exact: true })
    .check();
  await expect(
    page.getByRole("button", { name: "重试失败项", exact: true }),
  ).toBeEnabled();
  await page
    .getByRole("checkbox", { name: "选择任务 42", exact: true })
    .check();
  await page.getByRole("button", { name: "恢复任务", exact: true }).click();
  const dialog = page.getByRole("dialog", {
    name: "恢复 2 个任务？",
    exact: true,
  });
  await expect(dialog).toContainText(
    "失败任务重试失败或未完成的对象；已停止任务继续扫描和传输。已完成的对象不重复传输。",
  );
  await dialog.getByRole("button", { name: "确认恢复", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(sent).toHaveLength(1);
  expect(sent[0].taskIds.sort()).toEqual([41, 42]);
  expect(sent[0].expectedGenerations).toEqual({ "41": 3, "42": 5 });
});
