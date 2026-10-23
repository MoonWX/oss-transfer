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
  priorityOrder = false,
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
    const active = url.searchParams.get("view") === "active";
    const priority =
      active && priorityOrder && url.searchParams.get("order") === "priority";
    const rank: Record<string, number> = {
      RUNNING: 0,
      SCAN_COMPLETED: 0,
      RECOVERING: 1,
      RESUMING: 1,
      STOPPING: 2,
      RECOVERABLE: 3,
      WAITING_RECOVERY: 3,
      PENDING: 4,
      WAITING: 4,
      CREATED: 4,
    };
    const position = (item: Record<string, any>) =>
      `${rank[item.status] ?? 5}/${String(item.subtaskNumber).padStart(10, "0")}`;
    const cursor = url.searchParams.get("cursor") ?? "";
    const matching = numbered.filter(
      (item) =>
        (!node || (item.executionNodeIp || item.workerNode) === node) &&
        (!active ||
          [
            "CREATED",
            "PENDING",
            "WAITING",
            "RUNNING",
            "STOPPING",
            "RESUMING",
            "RECOVERING",
            "RECOVERABLE",
            "WAITING_RECOVERY",
            "SCAN_COMPLETED",
          ].includes(item.status)),
    );
    if (priority)
      matching.sort((a, b) => position(a).localeCompare(position(b)));
    const selected = match[2]
      ? matching.filter((item) => item.subtaskNumber === Number(match[2]))
      : matching
          .filter((item) =>
            priority ? position(item) > cursor : item.subtaskNumber > offset,
          )
          .slice(0, limit);
    const items = selected.map((item) => ({
      ...item,
      childTaskId: `${snapshot.parentTaskId}-${item.subtaskNumber}`,
    }));
    const nextOffset = priority
      ? offset + items.length
      : (items.at(-1)?.subtaskNumber ?? offset);
    const nextCursor = items.length ? position(items.at(-1)!) : cursor;
    const known = new Set(
      (url.searchParams.get("known") ?? "").split(",").map(Number),
    );
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
              items,
              totalCount: active
                ? matching.length
                : (snapshot.totalCount ??
                  Math.max(0, ...numbered.map((item) => item.subtaskNumber))),
              nextOffset,
              ...(priority
                ? {
                    order: "priority",
                    cursor,
                    nextCursor,
                    updates: numbered
                      .filter((item) => known.has(item.subtaskNumber))
                      .map((item) => ({
                        ...item,
                        childTaskId: `${snapshot.parentTaskId}-${item.subtaskNumber}`,
                      })),
                  }
                : {}),
              hasMore:
                !match[2] &&
                matching.some((item) =>
                  priority
                    ? position(item) > nextCursor
                    : item.subtaskNumber > nextOffset,
                ),
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
