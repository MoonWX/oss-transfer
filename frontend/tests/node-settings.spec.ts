import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

async function fixture(page: Page) {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "1"),
  );
  let reachable = true;
  let children = 16;
  let objects = 4;
  const roots = ["/mnt/data", "C:\\Archive"];
  const writes: Record<string, unknown>[] = [];
  await page.route("**/api/master/nodes**", async (route) => {
    if (route.request().method() === "PUT") {
      const body = route.request().postDataJSON();
      writes.push(body);
      if (body.maxConcurrentChildren !== undefined)
        children = body.maxConcurrentChildren;
      if (body.objectsPerChild !== undefined) objects = body.objectsPerChild;
    }
    await route.fulfill({
      json: {
        success: true,
        data: {
          nodes: [
            {
              address: "storage-node",
              label: "Storage",
              agent: true,
              online: reachable,
            },
          ],
          port: 1685,
        },
      },
    });
  });
  await page.route("**/api/master/worker-status", (route) =>
    route.fulfill({
      json: {
        success: true,
        workers: {
          "storage-node": reachable
            ? {
                success: true,
                executionStatus: {
                  activeScans: 1,
                  maxConcurrentScans: 2,
                  activeChildren: 4,
                  maxConcurrentChildren: children,
                  objectsPerChild: objects,
                  unfinishedChildren: 80,
                  maxUnfinishedChildren: 1024,
                },
                localStorage: { advertisedLocalRoots: roots },
              }
            : { success: false, message: "Agent disconnected" },
        },
      },
    }),
  );
  return {
    writes,
    offline: () => {
      reachable = false;
    },
  };
}

test("node workload remains visible and renaming preserves legacy directory settings", async ({
  page,
}) => {
  const state = await fixture(page);
  await page.goto("/nodes");
  const region = page.getByRole("region", {
    name: "执行节点 Storage",
    exact: true,
  });
  await expect(region.getByText("80 / 1,024", { exact: true })).toBeVisible();
  for (const label of ["扫描并发", "子任务并发", "未完成子任务"]) {
    await expect(
      region.getByRole("progressbar", { name: label, exact: true }),
    ).toBeVisible();
  }
  await expect(region.getByText("队列占用", { exact: true })).toHaveCount(0);
  await expect(region.getByRole("progressbar")).toHaveCount(3);
  await region.getByRole("button", { name: "编辑节点", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "编辑节点", exact: true });
  await expect(
    dialog.getByRole("textbox", { name: "节点标识", exact: true }),
  ).toHaveCount(0);
  await expect(dialog.getByLabel("本地根目录", { exact: true })).toHaveCount(0);
  await dialog.getByLabel("节点名称", { exact: true }).fill("Archive");
  await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(state.writes[0]).toEqual({
    address: "storage-node",
    label: "Archive",
  });
  await expect(region.getByText("本地根目录", { exact: true })).toHaveCount(0);
  await expect(region.getByText("/mnt/data", { exact: true })).toHaveCount(0);
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(region.getByText("80 / 1,024", { exact: true })).toBeVisible();
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(390);
});

test("renaming and unavailable-node editing do not send an empty root replacement", async ({
  page,
}) => {
  const state = await fixture(page);
  await page.goto("/nodes");
  const edit = page.getByRole("button", { name: "编辑节点", exact: true });
  await edit.click();
  let dialog = page.getByRole("dialog", { name: "编辑节点", exact: true });
  await dialog.getByLabel("节点名称", { exact: true }).fill("Renamed");
  await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(state.writes[0]).toEqual({
    address: "storage-node",
    label: "Renamed",
  });
  state.offline();
  await expect(
    page.getByText("Agent disconnected", { exact: true }),
  ).toBeVisible();
  await edit.click();
  dialog = page.getByRole("dialog", { name: "编辑节点", exact: true });
  await expect(dialog.getByLabel("本地根目录", { exact: true })).toHaveCount(0);
  await dialog.getByLabel("节点名称", { exact: true }).fill("Offline name");
  await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(state.writes[1]).toEqual({
    address: "storage-node",
    label: "Offline name",
  });
});

test("node concurrency applies through the settings dialog and is retained when reopened", async ({
  page,
}) => {
  const state = await fixture(page);
  await page.goto("/nodes");
  await page.getByRole("button", { name: "编辑节点", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "编辑节点", exact: true });
  await expect(
    dialog.getByRole("spinbutton", { name: "子任务并发", exact: true }),
  ).toHaveValue("16");
  await expect(
    dialog.getByRole("spinbutton", {
      name: "每个子任务的对象并发",
      exact: true,
    }),
  ).toHaveValue("4");
  await dialog
    .getByRole("spinbutton", { name: "子任务并发", exact: true })
    .fill("12");
  await dialog
    .getByRole("spinbutton", { name: "每个子任务的对象并发", exact: true })
    .fill("6");
  await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(state.writes).toEqual([
    {
      address: "storage-node",
      label: "Storage",
      maxConcurrentChildren: 12,
      objectsPerChild: 6,
    },
  ]);
  await expect(page.getByText("4 / 12", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "编辑节点", exact: true }).click();
  await expect(
    dialog.getByRole("spinbutton", { name: "子任务并发", exact: true }),
  ).toHaveValue("12");
  await expect(
    dialog.getByRole("spinbutton", {
      name: "每个子任务的对象并发",
      exact: true,
    }),
  ).toHaveValue("6");
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(
    dialog.getByRole("spinbutton", {
      name: "每个子任务的对象并发",
      exact: true,
    }),
  ).toBeVisible();
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(390);
});

test("invalid combined concurrency stays in the editor without submitting", async ({
  page,
}) => {
  const state = await fixture(page);
  await page.goto("/nodes");
  await page.getByRole("button", { name: "编辑节点", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "编辑节点", exact: true });
  await dialog
    .getByRole("spinbutton", { name: "子任务并发", exact: true })
    .fill("64");
  await dialog
    .getByRole("spinbutton", { name: "每个子任务的对象并发", exact: true })
    .fill("32");
  await dialog.getByRole("button", { name: "保存节点", exact: true }).click();
  await expect(dialog.getByRole("alert")).toContainText("两者乘积不能超过 256");
  expect(state.writes).toEqual([]);
});
