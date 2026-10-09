import type { Page } from "@playwright/test";
import { numberSubtasks } from "../src/lib/subtasks";

type Snapshot = {
  parentTaskId: number;
  items: Record<string, any>[];
  childTaskIds?: number[];
  partial?: boolean;
  error?: string;
  totalCount?: number;
};
export async function mockTaskChildren(
  page: Page,
  read: (id: number) => Snapshot | Promise<Snapshot>,
) {
  await page.route("**/api/master/tasks/*/children**", async (route) => {
    const url = new URL(route.request().url());
    const match = url.pathname.match(/\/tasks\/(\d+)\/children(?:\/(\d+))?$/)!;
    const snapshot = await read(Number(match[1]));
    const offset = match[2]
      ? Number(match[2]) - 1
      : Number(url.searchParams.get("offset") || 0);
    const limit = match[2] ? 1 : Number(url.searchParams.get("limit") || 25);
    const numbered = numberSubtasks(
      snapshot.items.filter((item) => item.shardId !== "discovery"),
      snapshot.childTaskIds,
    );
    const node = url.searchParams.get("executionNodeIp");
    await route.fulfill({
      status: snapshot.error ? 503 : 200,
      json: snapshot.error
        ? { success: false, message: snapshot.error }
        : {
            success: true,
            data: {
              parentTaskId: snapshot.parentTaskId,
              offset,
              limit,
              items: numbered
                .filter(
                  (item) =>
                    item.subtaskNumber > offset &&
                    item.subtaskNumber <= offset + limit &&
                    (!node ||
                      (item.executionNodeIp || item.workerNode) === node),
                )
                .map((item) => ({
                  ...item,
                  childTaskId: `${snapshot.parentTaskId}-${item.subtaskNumber}`,
                })),
              totalCount:
                snapshot.totalCount ??
                Math.max(0, ...numbered.map((item) => item.subtaskNumber)),
              partial: !!snapshot.partial,
              unreachableWorkers: snapshot.partial ? ["10.0.1.13"] : [],
            },
          },
    });
  });
}
export async function revealChildren(page: Page) {
  await page.getByTestId("task-children-region").scrollIntoViewIfNeeded();
}
