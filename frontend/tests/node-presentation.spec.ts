import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";
import { mockTaskChildren, revealChildren } from "./task-children-fixture";
import {
  displayNode,
  taskNodeLabels,
  resolveNodeDisplay,
  mergeNodeDisplay,
} from "../src/lib/nodeRegistry";
import { exceptionCsv } from "../src/lib/exceptionRecords";

const identity = "agent-4fddb1cb";
const node = {
  address: identity,
  id: "enrollment-a",
  label: "训练数据节点 A",
  displayName: "训练数据节点 A",
  displayAddress: "10.1.0.43",
  displayLabel: "训练数据节点 A (10.1.0.43)",
  identityOnly: false,
  agent: true,
  master: false,
  online: true,
};
const transfer = {
  ...tasks[0],
  taskIndex: 989,
  ossTaskGroup: true,
  workerNode: identity,
  executionNodes: [node],
};

async function setup(page: Page) {
  const requests = await mockApi(page);
  await page.route("**/api/master/nodes", (route) =>
    route.fulfill({ json: { success: true, data: { nodes: [node] } } }),
  );
  await page.route("**/api/master/worker-status", (route) =>
    route.fulfill({
      json: {
        success: true,
        workers: { [identity]: { success: true, ...node } },
      },
    }),
  );
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { tasks: [transfer], totalTasks: 1 } }),
  );
  return requests;
}

test("task and subtask names include real IPs while links keep the node identity", async ({
  page,
}) => {
  const requests = await setup(page);
  let registryReads = 0;
  page.on("request", (request) => {
    if (new URL(request.url()).pathname === "/api/master/nodes")
      registryReads++;
  });
  await mockTaskChildren(page, () => ({
    parentTaskId: 989,
    items: Array.from({ length: 50 }, (_, index) => ({
      ...transfer,
      taskIndex: 5000 + index,
      parentTaskId: 989,
      subtaskNumber: index + 1,
      childTaskId: `989-${index + 1}`,
      executionNodeIp: identity,
      status: "RUNNING",
    })),
  }));
  await page.goto("/tasks?taskId=989");
  await expect(
    page
      .getByRole("dialog")
      .getByText(node.displayLabel, { exact: true })
      .first(),
  ).toBeVisible();
  await revealChildren(page);
  await expect(page.locator('[data-child-id="989-1"]')).toContainText(
    node.displayLabel,
  );
  const readsBeforeScroll = registryReads;
  await page.getByTestId("task-children-more").scrollIntoViewIfNeeded();
  await expect(page.locator('[data-child-id="989-26"]')).toBeAttached();
  expect(registryReads).toBe(readsBeforeScroll);
  await page
    .getByTestId("task-children-region")
    .screenshot({ path: "/private/tmp/node-labels-subtasks.png" });
  await page
    .getByRole("button", { name: "查看子任务 989-26", exact: true })
    .click();
  await expect(page).toHaveURL(new RegExp(`childNode=${identity}`));
  await expect(
    page.getByRole("dialog").getByText(node.displayLabel, { exact: true }),
  ).toBeVisible();
  expect(
    requests.some((request) =>
      String(request.body).includes(node.displayLabel),
    ),
  ).toBe(false);
  await page.getByRole("button", { name: "关闭详情", exact: true }).click();
  await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
  await page
    .getByRole("option", { name: node.displayLabel, exact: true })
    .click();
  await expect(page.getByRole("combobox", { name: /^执行节点/ })).toContainText(
    node.displayLabel,
  );
  await expect(page.getByRole("row").filter({ hasText: "#989" })).toHaveCount(
    1,
  );
});

test("recycle and exception rows resolve names and preserve identity in CSV", async ({
  page,
}) => {
  await setup(page);
  const failure = {
    ...transfer,
    taskIndex: 989,
    parentTaskId: 989,
    childTaskId: "989-1",
    key: "sample.bin",
    errorMessage: "source missing",
  };
  await page.route("**/api/master/deleted-tasks", (route) =>
    route.fulfill({
      json: {
        deletedTasks: { [identity]: [{ ...transfer, status: "COMPLETED" }] },
        totalCount: 1,
        queriedWorkers: [identity],
        complete: true,
        nodeDisplays: { [identity]: node },
      },
    }),
  );
  await page.route("**/api/master/failed", (route) =>
    route.fulfill({
      json: {
        failedKeys: { [identity]: [failure], [`${identity}Count`]: 1 },
        queriedWorkers: [identity],
        complete: true,
        nodeDisplays: { [identity]: node },
      },
    }),
  );
  await page.goto("/tools");
  await page.getByRole("tab", { name: "任务回收站", exact: true }).click();
  await expect(
    page.getByRole("cell", { name: node.displayLabel, exact: true }),
  ).toBeVisible();
  await page.goto("/exceptions");
  await expect(
    page.getByText(node.displayLabel, { exact: true }).first(),
  ).toBeVisible();
  const csv = exceptionCsv([failure]);
  expect(csv).toContain(`"${node.displayLabel}"`);
  expect(csv.split("\r\n")[0]).toContain('"nodeId"');
  expect(csv.split("\r\n")[1]).toMatch(new RegExp(`"${identity}"$`));
});

test("node display compatibility never mistakes an opaque ID for an IP", () => {
  expect(
    displayNode(identity, { label: "训练节点", localIps: ["10.1.0.43"] }),
  ).toBe("训练节点 (10.1.0.43)");
  expect(displayNode("10.1.0.43", { label: "10.1.0.43" })).toBe("10.1.0.43");
  expect(displayNode(identity)).toContain(identity);
  expect(displayNode(identity)).not.toBe(identity);
  expect(
    taskNodeLabels({
      executionNodeIp: identity,
      executionNodeLabel: node.displayLabel,
    }),
  ).toBe(node.displayLabel);
});

test("current node names replace stale task snapshots and loopback connections use real interfaces", () => {
  expect(
    resolveNodeDisplay(identity, node, {
      displayLabel: "旧名字",
      identityOnly: true,
    }),
  ).toBe(node.displayLabel);
  expect(resolveNodeDisplay(identity, undefined, node)).toBe(node.displayLabel);
  expect(
    displayNode(identity, mergeNodeDisplay({ identityOnly: true }, node)),
  ).toBe(node.displayLabel);
  expect(
    displayNode(identity, mergeNodeDisplay({ label: "改名节点" }, node)),
  ).toBe("改名节点 (10.1.0.43)");
  expect(
    taskNodeLabels({
      taskIndex: 989,
      parentTaskId: 989,
      workerNode: identity,
      executionNodes: [
        node,
        { ...node, address: "another-agent", displayLabel: "另一个节点" },
      ],
    }),
  ).toBe(`${node.displayLabel}, 另一个节点`);
  for (const connectionAddress of [
    "127.0.0.2:1685",
    "::1",
    "[::1]:1685",
    "::ffff:127.0.0.1",
    "0.0.0.0",
    "::",
    "::ffff:0.0.0.0",
  ]) {
    expect(
      displayNode(identity, {
        label: "训练节点",
        connectionAddress,
        localIps: ["127.0.0.4", "::1", "10.1.0.43"],
      }),
    ).toBe("训练节点 (10.1.0.43)");
  }
  expect(
    taskNodeLabels({
      childTaskId: "989-1",
      executionNodeIp: identity,
      participatingWorkerNodes: [identity, "another-agent"],
      executionNodes: [
        node,
        { address: "another-agent", displayLabel: "另一个节点" },
      ],
    }),
  ).toBe(node.displayLabel);
});
