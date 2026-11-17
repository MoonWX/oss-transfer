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
