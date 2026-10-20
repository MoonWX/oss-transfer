import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

test("closing a failed connection test keeps its result throughout the fade", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/s3-config/production/buckets", (route) =>
    route.fulfill({
      status: 502,
      json: {
        success: false,
        message: "没有在线执行节点，请先添加执行节点再测试连接。",
      },
    }),
  );
  await page.goto("/storage");
  await page
    .getByRole("button", { name: "测试连接", exact: true })
    .first()
    .click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByRole("alert")).toHaveText(
    "没有在线执行节点，请先添加执行节点再测试连接。",
  );
  await dialog.evaluate((element) => {
    const frames: string[] = [];
    Object.assign(window, { connectionDialogFrames: frames });
    new MutationObserver(() => frames.push(element.textContent ?? "")).observe(
      element,
      { subtree: true, childList: true, characterData: true },
    );
  });
  await dialog.getByRole("button", { name: "关闭", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  const frames = await page.evaluate(
    () =>
      (window as Window & { connectionDialogFrames: string[] })
        .connectionDialogFrames,
  );
  expect(frames.join("\n")).not.toContain("已获取 0 个存储桶");
});

test("a dismissed pending request cannot replace a newly opened connection test", async ({
  page,
}) => {
  await mockApi(page);
  let resolveFirst!: () => void;
  let resolveSecond!: () => void;
  let finishFirst!: () => void;
  const firstGate = new Promise<void>((resolve) => (resolveFirst = resolve));
  const secondGate = new Promise<void>((resolve) => (resolveSecond = resolve));
  const firstFinished = new Promise<void>((resolve) => (finishFirst = resolve));
  await page.route(
    "**/api/master/s3-config/production/buckets",
    async (route) => {
      await firstGate;
      await route.fulfill({ json: { buckets: ["old-response"] } });
      finishFirst();
    },
  );
  await page.route("**/api/master/s3-config/backup/buckets", async (route) => {
    await secondGate;
    await route.fulfill({ json: { buckets: ["backup-bucket"] } });
  });
  await page.goto("/storage");
  await page
    .getByRole("button", { name: "测试连接", exact: true })
    .first()
    .click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("正在获取可访问的存储桶");
  await dialog.getByRole("button", { name: "关闭", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await page
    .getByRole("button", { name: "测试连接", exact: true })
    .nth(1)
    .click();
  await expect(dialog).toContainText("连接测试 · backup");
  resolveFirst();
  await firstFinished;
  await expect(dialog).toContainText("正在获取可访问的存储桶");
  await expect(dialog).not.toContainText("old-response");
  resolveSecond();
  await expect(dialog).toContainText("backup-bucket");
  await expect(dialog.getByRole("alert")).toHaveText("已获取 1 个存储桶。");
  await expect(dialog).not.toContainText("old-response");
});
