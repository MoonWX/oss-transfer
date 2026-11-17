import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

async function configureDownload(page: Page) {
  await page.goto("/create");
  await page.getByRole("button", { name: /对象存储 → 本地/ }).click();
  await page.getByRole("combobox", { name: "源存储连接", exact: true }).click();
  await page.getByRole("option", { name: "production", exact: true }).click();
  await page
    .getByRole("combobox", { name: "源存储桶", exact: true })
    .fill("media-archive");
  await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
  await page.getByRole("option", { name: /10\.0\.1\.12/ }).click();
  await page.getByLabel("目标目录绝对路径").fill("/mnt/data/restore");
}

// An Agent from before the write probe checks the path but not whether it
// can write there; the Master says so, and the console must not show a pass.
test("a download destination whose writability was not checked is not shown as passed", async ({
  page,
}) => {
  await mockApi(page);
  let olderAgent = true;
  await page.route("**/api/master/local-transfers/validate", (route) => {
    const body = route.request().postDataJSON();
    return route.fulfill({
      json: {
        success: true,
        data: {
          valid: true,
          nodeBindings: body[0].destinationEndpoint.nodeBindings.map(
            (node: object) => ({
              ...node,
              valid: true,
              pathType: "DIRECTORY",
              ...(olderAgent
                ? {
                    code: "LOCAL_PATH_WRITE_NOT_CHECKED",
                    message:
                      "执行节点版本较旧，未检查目录是否可写；升级该节点后重新预检即可检查",
                  }
                : { message: "目标目录可写，可用空间 1.2 TiB" }),
            }),
          ),
        },
      },
    });
  });
  await configureDownload(page);
  const preflight = page.getByRole("button", { name: "运行预检", exact: true });
  await preflight.click();
  // The precheck summary, not the notification that repeats its title.
  const result = page.getByRole("alert").filter({ hasText: "个节点配置" });
  await expect(result).toContainText("路径预检通过，未检查目录是否可写");
  await expect(result).toHaveClass(/MuiAlert-standardWarning/);
  await expect(page.getByText("节点预检通过", { exact: true })).toHaveCount(0);
  await expect(page.getByText("未检查写入", { exact: true })).toBeVisible();
  await expect(page.getByText("通过 · 目录", { exact: true })).toHaveCount(0);
  await expect(
    page.getByText(
      "执行节点版本较旧，未检查目录是否可写；升级该节点后重新预检即可检查",
      { exact: true },
    ),
  ).toBeVisible();
  // The precheck still allows creating the task, as before.
  await expect(
    page.getByRole("button", { name: "创建迁移任务", exact: true }),
  ).toBeEnabled();

  // A node that proved it can write passes as before.
  olderAgent = false;
  await page.getByLabel("目标目录绝对路径").fill("/mnt/data/restore2");
  await preflight.click();
  await expect(page.getByText("节点预检通过", { exact: true })).toBeVisible();
  await expect(page.getByText("通过 · 目录", { exact: true })).toBeVisible();
  await expect(page.getByText("未检查写入", { exact: true })).toHaveCount(0);
});
