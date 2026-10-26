import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";

function gate() {
  let release!: () => void;
  const promise = new Promise<void>((resolve) => {
    release = resolve;
  });
  return { promise, release };
}
const failedTask = () => ({
  ...tasks[2],
  status: "PARTIAL_FAILED",
  taskIndex: 1,
  generation: 1,
});
function acceptedResult(generation = 2) {
  return {
    success: true,
    data: {
      accepted: 1,
      ignored: 0,
      failed: 0,
      details: [
        {
          taskId: 1,
          status: "RESUMING",
          generation,
          success: true,
          accepted: true,
          ignored: false,
        },
      ],
    },
  };
}
async function failures(page: Page) {
  const failure = {
    parentTaskId: 1,
    taskIndex: 1,
    key: "failed.bin",
    errorMessage: "S3: HTTP 400",
    direction: "LOCAL_TO_OSS",
  };
  await page.route("**/api/master/local-transfers/failures?**", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          items: [failure, { ...failure, key: "another.bin" }],
          total: 2,
        },
      },
    }),
  );
}

for (const accepted of [true, false]) {
  test(`resume ${accepted ? "acceptance keeps the recovery lock" : "rejection releases the recovery lock"} without waiting for a stalled list refresh`, async ({
    page,
  }) => {
    await mockApi(page);
    await page.addInitScript(() =>
      localStorage.setItem("oss-transfer-refresh-seconds", "0"),
    );
    const task = failedTask();
    const submitted = gate(),
      refreshed = gate();
    let sent = 0,
      refreshing = false;
    await page.route("**/api/master/status", async (route) => {
      if (refreshing) await refreshed.promise;
      await route.fulfill({ json: { totalTasks: 1, tasks: [task] } });
    });
    await page.route("**/api/master/tasks/1", (route) =>
      route.fulfill({
        json: {
          success: true,
          data:
            accepted && refreshing
              ? { ...task, generation: 2, status: "RUNNING" }
              : task,
        },
      }),
    );
    await page.route("**/api/master/local-transfers/resume", async (route) => {
      sent++;
      expect(route.request().postDataJSON()).toEqual({
        taskIds: [1],
        expectedGenerations: { "1": 1 },
      });
      await submitted.promise;
      refreshing = true;
      await route.fulfill(
        accepted
          ? { json: acceptedResult() }
          : { status: 403, json: { message: "恢复暂不可用" } },
      );
    });
    try {
      await page.goto("/tasks?taskId=1");
      await page.getByRole("button", { name: "继续传输", exact: true }).click();
      const dialog = page.getByRole("dialog", { name: "继续传输 1 个任务？" });
      await dialog
        .getByRole("button", { name: "确认继续传输", exact: true })
        .dblclick();
      await expect(
        dialog.getByRole("button", { name: "提交中…", exact: true }),
      ).toBeDisabled();
      expect(sent).toBe(1);
      submitted.release();
      await expect(dialog).toBeHidden();
      // The stale list still says failed; only the accepted operation remains locked.
      const resume = page.getByRole("button", { name: /^(继续传输|处理中…)$/ });
      if (accepted) {
        await expect(resume).toBeDisabled();
        await expect(page.getByText(/^继续传输请求已提交/)).toBeVisible();
      } else {
        await expect(resume).toBeEnabled();
        await expect(
          page
            .getByRole("alert")
            .filter({ hasText: "部分请求未完成：恢复暂不可用" })
            .first(),
        ).toBeVisible();
      }
      expect(sent).toBe(1);
    } finally {
      submitted.release();
      refreshed.release();
    }
  });
}

test("exception recovery locks every row and survives navigation and reload until the next failed generation", async ({
  page,
}) => {
  await mockApi(page);
  await page.addInitScript(() =>
    localStorage.setItem("oss-transfer-refresh-seconds", "0"),
  );
  await failures(page);
  const task = failedTask();
  let latest = { ...task },
    sent = 0;
  const submitted = gate(),
    generations: number[] = [];
  // Deliberately stale list data: action state must not trust the old page.
  await page.route("**/api/master/status", (route) =>
    route.fulfill({ json: { totalTasks: 1, tasks: [task] } }),
  );
  await page.route("**/api/master/tasks/1", (route) =>
    route.fulfill({ json: { success: true, data: latest } }),
  );
  await page.route(
    "**/api/master/local-transfers/retry-failures",
    async (route) => {
      sent++;
      const body = route.request().postDataJSON();
      expect(body.taskIds).toEqual([1]);
      expect(body.expectedGenerations).toEqual({ "1": latest.generation });
      generations.push(body.expectedGenerations["1"]);
      await submitted.promise;
      latest = {
        ...task,
        generation: latest.generation + 1,
        status: "RUNNING",
      };
      await route.fulfill({ json: acceptedResult(latest.generation) });
    },
  );
  try {
    await page.goto("/exceptions?type=local&taskId=1");
    await page
      .getByRole("button", { name: "重试任务", exact: true })
      .first()
      .dblclick();
    const submitting = page.getByRole("button", {
      name: "提交中…",
      exact: true,
    });
    await expect(submitting).toHaveCount(2);
    await expect(submitting.first()).toBeDisabled();
    await expect(submitting.last()).toBeDisabled();
    await expect.poll(() => sent).toBe(1);
    submitted.release();
    const result = page
      .getByRole("alert")
      .filter({ hasText: "任务 #1 的重试已受理。" });
    await expect(result).toBeVisible();
    const processing = page.getByRole("button", {
      name: "处理中…",
      exact: true,
    });
    await expect(processing).toHaveCount(2);
    await expect(processing.first()).toBeDisabled();
    await expect(processing.last()).toBeDisabled();
    await expect(
      page.getByRole("button", { name: "完整迁移报告" }),
    ).toBeEnabled();
    await expect(
      page.getByRole("tab", { name: "失败对象", exact: true }),
    ).toBeEnabled();
    await result.getByRole("link", { name: "查看任务" }).click();
    await expect(page).toHaveURL(/\/tasks\?taskId=1$/);
    const detail = page.locator(".MuiDrawer-paper");
    await expect(detail).toBeVisible();
    await expect(
      detail.getByRole("button", { name: /^(继续传输|处理中…)$/ }),
    ).toBeDisabled();
    await page.goBack();
    await expect(processing).toHaveCount(2);
    await page.reload();
    await expect(processing).toHaveCount(2);
    await expect(processing.first()).toBeDisabled();
    expect(sent).toBe(1);
    // A new failure is retryable; the accepted lock must not be permanent.
    latest = { ...task, generation: 2, status: "PARTIAL_FAILED" };
    const retry = page.getByRole("button", { name: "重试任务", exact: true });
    await expect(retry).toHaveCount(2, { timeout: 10000 });
    await expect(retry.first()).toBeEnabled();
    await retry.first().click();
    await expect.poll(() => sent).toBe(2);
    await expect(processing).toHaveCount(2);
    expect(generations).toEqual([1, 2]);
  } finally {
    submitted.release();
  }
});

for (const surface of ["tasks", "exceptions"] as const) {
  test(`${surface} shows a definite rejection and permits another attempt`, async ({
    page,
  }) => {
    await mockApi(page);
    const task = failedTask();
    const reason = "节点 backup 已被移除，请重新添加节点后创建任务。";
    await page.route("**/api/master/status", (route) =>
      route.fulfill({ json: { totalTasks: 1, tasks: [task] } }),
    );
    await page.route("**/api/master/tasks/1", (route) =>
      route.fulfill({ json: { success: true, data: task } }),
    );
    await failures(page);
    const action = surface === "tasks" ? "resume" : "retry-failures";
    let sent = 0;
    await page.route(`**/api/master/local-transfers/${action}`, (route) => {
      sent++;
      expect(route.request().postDataJSON()).toEqual({
        taskIds: [1],
        expectedGenerations: { "1": 1 },
      });
      return route.fulfill({
        json: {
          success: true,
          data: {
            accepted: 0,
            ignored: 0,
            failed: 1,
            partial: true,
            details: [
              {
                taskId: 1,
                generation: 1,
                status: "PARTIAL_FAILED",
                success: false,
                accepted: false,
                ignored: false,
                message: reason,
              },
            ],
          },
        },
      });
    });
    await page.goto(
      surface === "tasks"
        ? "/tasks?taskId=1"
        : "/exceptions?type=local&taskId=1",
    );
    const submit = async () => {
      if (surface === "tasks") {
        await page
          .getByRole("button", { name: "继续传输", exact: true })
          .click();
        await page
          .getByRole("button", { name: "确认继续传输", exact: true })
          .click();
        await expect(
          page.getByRole("dialog", { name: "继续传输 1 个任务？" }),
        ).toBeHidden();
        await expect(
          page.getByRole("button", { name: "继续传输", exact: true }),
        ).toBeEnabled();
      } else {
        const retry = page
          .getByRole("button", { name: "重试任务", exact: true })
          .first();
        await retry.click();
        await expect(retry).toBeEnabled();
      }
    };
    await submit();
    await expect(
      page.getByRole("alert").filter({ hasText: reason }).first(),
    ).toBeVisible();
    await expect(page.getByText(/不可达节点：未知/)).toHaveCount(0);
    await expect(
      page.getByText("任务 #1 的重试已受理。", { exact: true }),
    ).toHaveCount(0);
    await expect(page.getByText(/^继续传输请求已提交/)).toHaveCount(0);
    await submit();
    expect(sent).toBe(2);
  });
}

test("a lost retry response is reconciled before permitting another submission", async ({
  page,
}) => {
  await mockApi(page);
  await failures(page);
  const task = failedTask();
  let latest = { ...task },
    sent = 0;
  await page.route("**/api/master/tasks/1", (route) =>
    route.fulfill({ json: { success: true, data: latest } }),
  );
  await page.route(
    "**/api/master/local-transfers/retry-failures",
    async (route) => {
      sent++;
      latest = { ...task, generation: 2, status: "RUNNING" };
      // The server accepted the operation, but its response did not arrive.
      await route.abort("failed");
    },
  );
  await page.goto("/exceptions?type=local&taskId=1");
  await page
    .getByRole("button", { name: "重试任务", exact: true })
    .first()
    .click();
  const processing = page.getByRole("button", { name: "处理中…", exact: true });
  await expect(processing).toHaveCount(2);
  await expect(processing.first()).toBeDisabled();
  await page.reload();
  await expect(processing).toHaveCount(2);
  await expect(processing.last()).toBeDisabled();
  expect(sent).toBe(1);
  latest = { ...task, generation: 2, status: "PARTIAL_FAILED" };
  await expect(
    page.getByRole("button", { name: "重试任务", exact: true }),
  ).toHaveCount(2, { timeout: 10000 });
  expect(sent).toBe(1);
});

test("a retry preflight from an old login cannot submit using the new login", async ({
  page,
}) => {
  await mockApi(page);
  await failures(page);
  const preflight = gate();
  let started = false,
    sent = 0,
    reads = 0;
  await page.route("**/api/master/tasks/1", async (route) => {
    if (
      ++reads === 1 ||
      route.request().headers().authorization !== "Bearer test-token"
    ) {
      await route.fulfill({ json: { success: true, data: failedTask() } });
      return;
    }
    expect(route.request().headers().authorization).toBe("Bearer test-token");
    started = true;
    await preflight.promise;
    await route.fulfill({ json: { success: true, data: failedTask() } });
  });
  await page.route("**/api/master/local-transfers/retry-failures", (route) => {
    sent++;
    return route.fulfill({ json: acceptedResult() });
  });
  try {
    await page.goto("/exceptions?type=local&taskId=1");
    await page
      .getByRole("button", { name: "重试任务", exact: true })
      .first()
      .click();
    await expect.poll(() => started).toBe(true);
    await page.evaluate(() =>
      sessionStorage.setItem("oss-transfer-token", "new-login-token"),
    );
    preflight.release();
    // The preflight has returned and the handler has completed its finally block.
    await expect(
      page.getByRole("button", { name: "完整迁移报告" }),
    ).toBeEnabled();
    expect(sent).toBe(0);
    expect(
      await page.evaluate(() => sessionStorage.getItem("oss-transfer-token")),
    ).toBe("new-login-token");
  } finally {
    preflight.release();
  }
});

test("a delayed 401 from recovery polling cannot clear a newer login", async ({
  page,
}) => {
  await mockApi(page);
  await failures(page);
  const polling = gate();
  let accepted = false,
    pollStarted = false;
  await page.route("**/api/master/tasks/1", async (route) => {
    if (!accepted) {
      await route.fulfill({ json: { success: true, data: failedTask() } });
      return;
    }
    if (route.request().headers().authorization === "Bearer new-login-token") {
      await route.fulfill({
        json: {
          success: true,
          data: { ...failedTask(), status: "RUNNING", generation: 2 },
        },
      });
      return;
    }
    expect(route.request().headers().authorization).toBe("Bearer test-token");
    pollStarted = true;
    await polling.promise;
    await route.fulfill({ status: 401, json: { message: "Session expired" } });
  });
  await page.route("**/api/master/local-transfers/retry-failures", (route) => {
    accepted = true;
    return route.fulfill({ json: acceptedResult() });
  });
  try {
    await page.goto("/exceptions?type=local&taskId=1");
    await page
      .getByRole("button", { name: "重试任务", exact: true })
      .first()
      .click();
    await expect.poll(() => pollStarted).toBe(true);
    await page.evaluate(() =>
      sessionStorage.setItem("oss-transfer-token", "new-login-token"),
    );
    const responseReceived = page.waitForResponse(
      (response) =>
        response.url().endsWith("/api/master/tasks/1") &&
        response.status() === 401,
    );
    polling.release();
    await (await responseReceived).finished();
    // A subsequent UI request must still authenticate as the new login.
    const refreshed = page.waitForRequest((request) =>
      request.url().includes("/api/master/local-transfers/failures?"),
    );
    await page.getByRole("button", { name: "刷新", exact: true }).click();
    expect((await refreshed).headers().authorization).toBe(
      "Bearer new-login-token",
    );
    expect(
      await page.evaluate(() => sessionStorage.getItem("oss-transfer-token")),
    ).toBe("new-login-token");
    await expect(page).toHaveURL(/\/exceptions\?type=local&taskId=1$/);
  } finally {
    polling.release();
  }
});

for (const status of ["COMPLETED", "RUNNING"] as const) {
  test(`a fresh exception page disables historical failures for a ${status} task`, async ({
    page,
  }) => {
    await mockApi(page);
    await failures(page);
    const inspected = gate();
    let sent = 0;
    await page.route("**/api/master/tasks/1", async (route) => {
      await inspected.promise;
      await route.fulfill({
        json: {
          success: true,
          data: { ...failedTask(), status, generation: 2 },
        },
      });
    });
    await page.route(
      "**/api/master/local-transfers/retry-failures",
      async (route) => {
        sent++;
        await route.fulfill({ json: acceptedResult() });
      },
    );
    try {
      await page.goto("/exceptions?type=local&taskId=1");
      const checking = page.getByRole("button", {
        name: "检查状态…",
        exact: true,
      });
      await expect(checking).toHaveCount(2);
      await expect(checking.first()).toBeDisabled();
      inspected.release();
      const action = page.getByRole("button", {
        name: status === "COMPLETED" ? "已完成" : "处理中…",
        exact: true,
      });
      await expect(action).toHaveCount(2);
      await expect(action.first()).toBeDisabled();
      await expect(action.last()).toBeDisabled();
      await expect(
        page.getByRole("button", { name: "重试任务", exact: true }),
      ).toHaveCount(0);
      expect(sent).toBe(0);
    } finally {
      inspected.release();
    }
  });
}

test("an older exception tab cannot silently retry a newer failed generation", async ({
  page,
  context,
}) => {
  const second = await context.newPage();
  let latest = failedTask(),
    sent = 0;
  for (const tab of [page, second]) {
    await mockApi(tab);
    await failures(tab);
    await tab.route("**/api/master/tasks/1", (route) =>
      route.fulfill({ json: { success: true, data: latest } }),
    );
    await tab.route("**/api/master/local-transfers/retry-failures", (route) => {
      sent++;
      expect(route.request().postDataJSON().expectedGenerations).toEqual({
        "1": 1,
      });
      latest = { ...failedTask(), generation: 2 };
      return route.fulfill({ json: acceptedResult(2) });
    });
    await tab.goto("/exceptions?type=local&taskId=1");
    await expect(
      tab.getByRole("button", { name: "重试任务", exact: true }).first(),
    ).toBeEnabled();
  }
  await page
    .getByRole("button", { name: "重试任务", exact: true })
    .first()
    .click();
  await expect.poll(() => sent).toBe(1);
  await second
    .getByRole("button", { name: "重试任务", exact: true })
    .first()
    .click();
  await expect(
    second
      .getByRole("alert")
      .filter({ hasText: "任务状态已更新，请查看任务详情。" }),
  ).toBeVisible();
  expect(sent).toBe(1);
  await second.close();
});

test("an unavailable task status disables retry and recovers on the next status check", async ({
  page,
}) => {
  await mockApi(page);
  await failures(page);
  let unavailable = true,
    sent = 0;
  await page.route("**/api/master/tasks/1", (route) =>
    route.fulfill(
      unavailable
        ? { status: 503, json: { message: "Temporarily unavailable" } }
        : { json: { success: true, data: failedTask() } },
    ),
  );
  await page.route("**/api/master/local-transfers/retry-failures", (route) => {
    sent++;
    return route.fulfill({ json: acceptedResult() });
  });
  await page.goto("/exceptions?type=local&taskId=1");
  const action = page.getByRole("button", {
    name: "状态暂不可用",
    exact: true,
  });
  await expect(action).toHaveCount(2);
  await expect(action.first()).toBeDisabled();
  unavailable = false;
  await expect(
    page.getByRole("button", { name: "重试任务", exact: true }).first(),
  ).toBeEnabled({ timeout: 10000 });
  expect(sent).toBe(0);
});
