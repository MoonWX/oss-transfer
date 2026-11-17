import { expect, test } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

// After a Master restart, a node that stayed down keeps its last connection.
// A node with no record has not connected since the Master started; it is
// not described as one that never connected.
test("an offline node without a known last connection is not called never connected", async ({
  page,
}) => {
  await mockApi(page);
  const lastSeen = Date.parse("2026-11-16T22:05:00Z");
  const started = Date.parse("2026-11-17T08:30:00Z");
  await page.route("**/api/master/tasks/1048", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          ...tasks[0],
          offlineNodes: [
            {
              executionNodeIp: "10.0.1.12",
              displayLabel: "agent-a (10.0.1.12)",
              lastSeen,
            },
            {
              executionNodeIp: "10.0.1.13",
              displayLabel: "agent-b (10.0.1.13)",
              notConnectedSince: started,
            },
          ],
          attention: {
            code: "NODES_OFFLINE",
            severity: "warning",
            message:
              "执行节点 agent-a (10.0.1.12)、agent-b (10.0.1.13) 离线，任务等待其恢复连接",
          },
        },
      },
    }),
  );
  await page.goto("/tasks?taskId=1048");
  const attention = page.getByTestId("task-attention");
  await expect(attention).toContainText("agent-a (10.0.1.12) 最后连接于");
  await expect(attention).toContainText(
    /agent-b \(10\.0\.1\.13\) 自 Master 启动（.+）以来未连接/,
  );
  await expect(attention).not.toContainText("尚未连接");
});
