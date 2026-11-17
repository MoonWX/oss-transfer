import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

// "Failed objects" is described as transfers between object storages, next to
// a separate "Local transfer failures" tab. Its records, task picker and
// cleanup therefore ask for OSS_TO_OSS only, so a local failure is neither
// counted on both tabs nor cleared from the wrong one.
test("the failed-objects tab lists only transfers between object storages", async ({
  page,
}) => {
  const requests = await mockApi(page);
  const catalogs: URLSearchParams[] = [];
  await page.route("**/api/master/exceptions/tasks?**", (route) => {
    catalogs.push(new URL(route.request().url()).searchParams);
    return route.fulfill({
      json: { data: { items: [{ taskIndex: 41 }], hasMore: false } },
    });
  });
  await page.goto("/exceptions");
  await expect(
    page.getByRole("tab", { name: "失败对象", selected: true }),
  ).toBeVisible();
  await expect
    .poll(() => requests.filter((item) => item.path === "/api/master/failed"))
    .not.toHaveLength(0);
  for (const item of requests.filter(
    (request) => request.path === "/api/master/failed",
  ))
    expect(item.body.direction).toBe("OSS_TO_OSS");
  await page.getByRole("combobox", { name: "所属任务", exact: true }).click();
  await expect(
    page.getByRole("option", { name: "任务 #41", exact: true }),
  ).toBeVisible();
  expect(catalogs.at(-1)?.get("type")).toBe("failed");
  expect(catalogs.at(-1)?.get("direction")).toBe("OSS_TO_OSS");

  // The local tab keeps both local directions.
  await page.keyboard.press("Escape");
  await page.getByRole("tab", { name: "本地迁移失败" }).click();
  await expect
    .poll(() =>
      requests.filter((item) =>
        item.path.startsWith("/api/master/local-transfers/failures"),
      ),
    )
    .not.toHaveLength(0);
  for (const item of requests.filter((request) =>
    request.path.startsWith("/api/master/local-transfers/failures"),
  ))
    expect(new URL(item.path, "http://x").searchParams.has("direction")).toBe(
      false,
    );
});

// The local report covers every ended or stopped local task, whether or not
// it is complete. Its button names that scope instead of a "full report",
// and the caption says where completeness is recorded.
test("the local report button names its scope, not completeness", async ({
  page,
}) => {
  await mockApi(page);
  await page.goto("/exceptions?type=local");
  await expect(
    page.getByRole("button", { name: "导出全部本地任务报告", exact: true }),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: /完整迁移报告/ })).toHaveCount(
    0,
  );
  await expect(
    page.getByText(
      "报告包含所有已结束或已停止的本地迁移任务，不受当前筛选条件影响。报告是否完整记录在 report-metadata.json 的 complete 中，不完整时（例如有节点不可达或对象未处理）同时列出原因。",
      { exact: true },
    ),
  ).toBeVisible();
});
