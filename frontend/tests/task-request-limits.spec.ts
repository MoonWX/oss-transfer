import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

test("editing task request caps sends all limits and reopening shows confirmed values", async ({
  page,
}) => {
  await mockApi(page);
  let current = {
    limitMbps: 100 as number | null,
    requestLimits: {
      scope: "CLUSTER",
      listQps: 10,
      headQps: 20,
      putQps: 30,
    } as Record<string, unknown> | null,
    applied: true,
    sharedTaskCount: 1,
    canEdit: true,
    nodes: [],
  };
  const writes: unknown[] = [];
  await page.route(
    "**/api/master/tasks/1048/bandwidth-limit",
    async (route) => {
      if (route.request().method() === "PUT") {
        const body = route.request().postDataJSON();
        writes.push(body);
        current = { ...current, ...body };
      }
      await route.fulfill({ json: { success: true, data: current } });
    },
  );
  await page.goto("/tasks?taskId=1048");
  await page.getByRole("button", { name: "调整", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "任务 #1048 限速" });
  await expect(dialog.getByLabel("HEAD QPS")).toHaveValue("20");
  await dialog.getByLabel("HEAD QPS").fill("-1");
  await expect(dialog.getByText(/QPS 必须/)).toBeVisible();
  expect(writes).toHaveLength(0);
  await dialog.getByLabel("HEAD QPS").fill("50");
  await dialog.getByLabel("PUT QPS").fill("60");
  await dialog.getByRole("button", { name: "应用", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(writes).toEqual([
    {
      limitMbps: 100,
      requestLimits: { scope: "CLUSTER", listQps: 10, headQps: 50, putQps: 60 },
    },
  ]);
  await page.getByRole("button", { name: "调整", exact: true }).click();
  await expect(dialog.getByLabel("HEAD QPS")).toHaveValue("50");
  for (const field of ["LIST / GET QPS", "HEAD QPS", "PUT QPS"])
    await dialog.getByLabel(field).fill("");
  await dialog.getByRole("button", { name: "应用", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(writes.at(-1)).toEqual({ limitMbps: 100, requestLimits: null });
});

test("a lost write response cannot confirm success from bandwidth alone", async ({
  page,
}) => {
  await mockApi(page);
  const current = {
    limitMbps: 100,
    requestLimits: { scope: "CLUSTER", putQps: 30 },
    applied: true,
    sharedTaskCount: 1,
    canEdit: true,
    nodes: [],
  };
  await page.route(
    "**/api/master/tasks/1048/bandwidth-limit",
    async (route) => {
      if (route.request().method() === "PUT") {
        await route.abort();
        return;
      }
      await route.fulfill({ json: { success: true, data: current } });
    },
  );
  await page.goto("/tasks?taskId=1048");
  await page.getByRole("button", { name: "调整", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "任务 #1048 限速" });
  await dialog.getByLabel("PUT QPS").fill("60");
  await dialog.getByRole("button", { name: "应用", exact: true }).click();
  await expect(dialog.getByText(/应用结果未确认/)).toBeVisible();
  await expect(dialog).toBeVisible();
});
