import { expect, test } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

// The Master totals only its most recent page of tasks. The overview cards
// say so instead of presenting those sums as the totals of every task.
test("the overview says when its statistics leave out earlier tasks", async ({
  page,
}) => {
  await mockApi(page);
  let truncated = true;
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: {
        success: true,
        totalTasks: tasks.length,
        tasks,
        truncated,
        hasMore: truncated,
        ...(truncated ? { nextBeforeId: 1045 } : {}),
      },
    }),
  );
  await page.goto("/");
  const notice = page.getByTestId("overview-truncated");
  await expect(notice).toContainText(
    "以下统计仅包含最近 4 个任务，更早的任务未计入。",
  );
  await expect(notice.getByRole("link", { name: "查看全部" })).toHaveAttribute(
    "href",
    "/tasks",
  );
  truncated = false;
  await page.getByRole("button", { name: "刷新任务" }).click();
  await expect(notice).toHaveCount(0);
});
