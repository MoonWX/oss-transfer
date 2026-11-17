import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

async function select(page: Page, label: string, option: string) {
  await page.getByRole("combobox", { name: label }).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}

const row = {
  sourceS3Alias: "production",
  sourceBucket: "media-archive",
  destS3Alias: "backup",
  destBucket: "media-backup",
  startKey: "photos/2024/",
  endKey: "photos/2025/",
};

// The scanner never reads a marker: an imported subtask scans its range from
// startKey again. The page must not offer it as a checkpoint to resume from.
test("imported subtasks are not described as resuming from a marker, and a marker is called out", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.goto("/create");
  await page.getByRole("tab", { name: "批量导入" }).click();
  await select(page, "导入类型", "OSS 子任务恢复 / 导入");
  await expect(
    page.getByText(
      "用于按 Key 范围导入 OSS 子任务。支持 startKey、endKey 和 workerNode，留空 workerNode 时由集群分配。每个子任务从 startKey 之后完整扫描其范围，不会从断点继续；目标端已存在的对象按冲突策略处理。",
      { exact: true },
    ),
  ).toBeVisible();
  await expect(page.getByText(/恢复断点/)).toHaveCount(0);
  const warning = page.getByTestId("batch-marker-warning");
  await page.getByLabel("任务数据").fill(JSON.stringify([row]));
  await expect(warning).toHaveCount(0);
  await page
    .getByLabel("任务数据")
    .fill(
      JSON.stringify([
        row,
        { ...row, marker: "photos/2024/06/a.jpg" },
        { ...row, marker: "" },
        { ...row, marker: "photos/2024/09/b.jpg" },
      ]),
    );
  await expect(warning).toHaveText(
    "第 2, 4 项设置了 marker，marker 不会被使用：这些子任务从 startKey 之后完整扫描各自的范围，不会从断点继续；目标端已存在的对象按冲突策略处理。",
  );
  // The import itself is unchanged: the rows are sent as they are.
  await page.getByRole("button", { name: "导入任务", exact: true }).click();
  const imports = () =>
    requests.filter((request) => request.path === "/api/master/import");
  await expect.poll(() => imports().length).toBe(1);
  expect(imports()[0].body[1].marker).toBe("photos/2024/06/a.jpg");
});
