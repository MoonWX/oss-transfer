import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

const row = (taskIndex: number, retryStatus: string) => ({
  taskIndex,
  parentTaskId: taskIndex,
  key: `file-${taskIndex}.bin`,
  errorMessage: "Upload failed",
  retryStatus,
  direction: "OSS_TO_OSS",
});

for (const clearableCount of [undefined, 0]) {
  test(`unresolved records cannot be cleared when clearableCount is ${clearableCount}`, async ({
    page,
  }) => {
    const requests = await mockApi(page);
    await page.route("**/api/master/failed", (route) =>
      route.fulfill({
        json: {
          success: true,
          clearableCount,
          failedKeys: { "10.0.1.12": [row(41, "FAILED")], "10.0.1.12Count": 1 },
        },
      }),
    );
    await page.goto("/exceptions");
    await expect(page.getByText("file-41.bin", { exact: true })).toBeVisible();
    await expect(
      page.getByRole("button", { name: "清理已处理记录", exact: true }),
    ).toBeDisabled();
    expect(requests.some((request) => request.path.endsWith("/clear"))).toBe(
      false,
    );
  });
}

test("cleanup uses applied task, bucket and node filters, includes resolved records beyond the loaded page", async ({
  page,
}) => {
  await mockApi(page);
  const submissions: unknown[] = [];
  let cleared = false;
  await page.route("**/api/master/failed", (route) =>
    route.fulfill({
      json: {
        success: true,
        clearableCount: cleared ? 0 : 2000,
        failedKeys: {
          "10.0.1.12": [row(41, "FAILED")],
          "10.0.1.12Count": cleared ? 1 : 2001,
        },
      },
    }),
  );
  await page.route("**/api/master/failed/clear", (route) => {
    submissions.push(route.request().postDataJSON());
    cleared = true;
    return route.fulfill({
      json: { success: true, clearedCount: 2000, complete: true },
    });
  });
  await page.goto("/exceptions?taskId=41");
  await page
    .getByRole("textbox", { name: "源 Bucket", exact: true })
    .fill("source");
  await page
    .getByRole("textbox", { name: "目标 Bucket", exact: true })
    .fill("target");
  await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
  await page
    .getByRole("option", { name: "master (10.0.1.12)", exact: true })
    .click();
  await page.getByRole("button", { name: "应用筛选", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "清理已处理记录", exact: true }),
  ).toBeEnabled();
  // Editing a field without applying it must not broaden or change cleanup.
  await page
    .getByRole("textbox", { name: "源 Bucket", exact: true })
    .fill("unsaved-draft");
  await page
    .getByRole("button", { name: "清理已处理记录", exact: true })
    .click();
  const dialog = page.getByRole("dialog", {
    name: "清理已处理记录？",
    exact: true,
  });
  await expect(dialog).toContainText("2,000 条已恢复或已跳过");
  await expect(dialog).toContainText("任务和未解决的失败记录会保留");
  await dialog.getByRole("button", { name: "取消", exact: true }).click();
  expect(submissions).toHaveLength(0);
  await page
    .getByRole("button", { name: "清理已处理记录", exact: true })
    .click();
  await dialog.getByRole("button", { name: "确认清理", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(submissions).toEqual([
    {
      type: "failed",
      taskIndex: 41,
      sourceBucket: "source",
      destBucket: "target",
      executionNodeIp: "10.0.1.12",
    },
  ]);
  await expect(
    page.getByText("已清理 2,000 条已处理记录。", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText("file-41.bin", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("button", { name: "清理已处理记录", exact: true }),
  ).toBeDisabled();
});

test("local cleanup uses direction and endpoint filters without deleting the task", async ({
  page,
}) => {
  const requests = await mockApi(page);
  let cleared = false;
  await page.route("**/api/master/local-transfers/failures?**", (route) =>
    route.fulfill({
      json: {
        success: true,
        data: {
          items: cleared ? [] : [row(52, "SKIPPED")],
          total: cleared ? 0 : 1,
          clearableCount: cleared ? 0 : 1,
        },
      },
    }),
  );
  const submissions: unknown[] = [];
  await page.route("**/api/master/failed/clear", (route) => {
    submissions.push(route.request().postDataJSON());
    cleared = true;
    return route.fulfill({ json: { success: true, clearedCount: 1 } });
  });
  await page.goto("/exceptions?type=local&taskId=52");
  await page.getByRole("combobox", { name: "迁移方向", exact: true }).click();
  await page.getByRole("option", { name: "本地 → OSS", exact: true }).click();
  await page
    .getByRole("textbox", { name: "端点 / 路径", exact: true })
    .fill("/mnt/source");
  await page.getByRole("button", { name: "应用筛选", exact: true }).click();
  await page
    .getByRole("button", { name: "清理已处理记录", exact: true })
    .click();
  await page.getByRole("button", { name: "确认清理", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "暂无匹配记录", exact: true }),
  ).toBeVisible();
  expect(submissions).toEqual([
    {
      type: "local",
      taskIndex: 52,
      direction: "LOCAL_TO_OSS",
      endpoint: "/mnt/source",
    },
  ]);
  expect(requests.some((request) => /delete|remove/.test(request.path))).toBe(
    false,
  );
});

test("task choices only use exception membership and refresh after the last resolved record is cleared", async ({
  page,
}) => {
  const requests = await mockApi(page);
  let cleared = false;
  const searches: URL[] = [];
  await page.route("**/api/master/exceptions/tasks?**", (route) => {
    const url = new URL(route.request().url());
    searches.push(url);
    const search = url.searchParams.get("search") || "";
    return route.fulfill({
      json: {
        data: {
          items: (cleared ? [42] : [41, 42])
            .filter((id) => String(id).includes(search))
            .map((taskIndex) => ({ taskIndex })),
          hasMore: false,
        },
      },
    });
  });
  await page.route("**/api/master/failed", (route) =>
    route.fulfill({
      json: {
        success: true,
        clearableCount: cleared ? 0 : 1,
        failedKeys: {
          "10.0.1.12": cleared ? [] : [row(41, "RECOVERED")],
          "10.0.1.12Count": cleared ? 0 : 1,
        },
      },
    }),
  );
  await page.route("**/api/master/failed/clear", (route) => {
    expect(route.request().postDataJSON()).toEqual({
      type: "failed",
      taskIndex: 41,
    });
    cleared = true;
    return route.fulfill({ json: { success: true, clearedCount: 1 } });
  });
  await page.goto("/exceptions");
  const input = page.getByRole("combobox", { name: "所属任务", exact: true });
  await input.click();
  await expect(
    page.getByRole("option", { name: "任务 #41", exact: true }),
  ).toBeVisible();
  await expect(page.getByRole("option", { name: /104[6-9]/ })).toHaveCount(0);
  await page.getByRole("option", { name: "任务 #41", exact: true }).click();
  await page
    .getByRole("button", { name: "清理已处理记录", exact: true })
    .click();
  await page.getByRole("button", { name: "确认清理", exact: true }).click();
  await expect(page.getByRole("dialog")).toBeHidden();
  await input.fill("");
  await input.press("ArrowDown");
  await expect(
    page.getByRole("option", { name: "任务 #42", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("option", { name: "任务 #41", exact: true }),
  ).toHaveCount(0);
  expect(searches.every((url) => url.searchParams.get("limit") === "100")).toBe(
    true,
  );
  expect(
    requests.some((request) => request.path === "/api/master/status"),
  ).toBe(false);
  // A direct task link remains readable even when there are no visible records.
  await page.goto("/exceptions?taskId=99");
  await expect(input).toHaveValue("99");
  await expect(
    page.getByRole("heading", { name: "暂无匹配记录", exact: true }),
  ).toBeVisible();
});

test("late task-search responses cannot mix old input or tab options", async ({
  page,
}) => {
  await mockApi(page);
  const delayed: Array<() => Promise<void>> = [];
  const searches: string[] = [];
  await page.route("**/api/master/exceptions/tasks?**", async (route) => {
    const url = new URL(route.request().url());
    const type = url.searchParams.get("type");
    const search = url.searchParams.get("search") || "";
    searches.push(`${type}:${search}`);
    const respond = () =>
      route
        .fulfill({
          json: {
            data: {
              items: [
                { taskIndex: type === "local" ? 77 : Number(search || 41) },
              ],
              hasMore: false,
            },
          },
        })
        .catch(() => {});
    if (type === "failed" && (search === "1" || search === "3"))
      delayed.push(respond);
    else await respond();
  });
  await page.goto("/exceptions");
  const input = page.getByRole("combobox", { name: "所属任务", exact: true });
  await input.fill("1");
  await expect.poll(() => searches.includes("failed:1")).toBe(true);
  await input.fill("2");
  await expect(
    page.getByRole("option", { name: "任务 #2", exact: true }),
  ).toBeVisible();
  await delayed.shift()!();
  await expect(
    page.getByRole("option", { name: "任务 #1", exact: true }),
  ).toHaveCount(0);
  await input.fill("3");
  await expect.poll(() => searches.includes("failed:3")).toBe(true);
  await input.press("Escape");
  await page.getByRole("tab", { name: "本地迁移失败", exact: true }).click();
  await input.click();
  await expect(
    page.getByRole("option", { name: "任务 #77", exact: true }),
  ).toBeVisible();
  await delayed.shift()!();
  await expect(
    page.getByRole("option", { name: "任务 #3", exact: true }),
  ).toHaveCount(0);
  expect(searches).toContain("local:");
});

test("task ID search waits for a valid ID and accepts a hash prefix", async ({
  page,
}) => {
  await mockApi(page);
  const searches: string[] = [];
  await page.route("**/api/master/exceptions/tasks?**", (route) => {
    searches.push(
      new URL(route.request().url()).searchParams.get("search") || "",
    );
    return route.fulfill({
      json: { data: { items: [{ taskIndex: 41 }], hasMore: false } },
    });
  });
  await page.goto("/exceptions");
  const input = page.getByRole("combobox", { name: "所属任务", exact: true });
  for (const invalid of ["#", "abc"]) {
    await input.fill(invalid);
    // Allow the search debounce to expire: unfinished IDs must not reach the API.
    await page.waitForTimeout(300);
    expect(searches).toHaveLength(0);
  }
  await input.fill("#41");
  await page.getByRole("option", { name: "任务 #41", exact: true }).click();
  await expect(input).toHaveValue("41");
  expect(searches).toEqual(["41"]);
});

for (const width of [1280, 390]) {
  test(`task dropdown stays compact and readable with long paths at ${width}px`, async ({
    page,
  }) => {
    await page.setViewportSize({ width, height: 900 });
    const requests = await mockApi(page);
    const source = "/mnt/shared/training-datasets/2026-10-30/images";
    const destination =
      "/mnt/backups/a-very-long-directory-name/2026-10-30/corpus/snapshots";
    await page.route("**/api/master/exceptions/tasks?**", (route) =>
      route.fulfill({
        json: {
          data: {
            items: Array.from({ length: 20 }, (_, index) => ({
              taskIndex: index + 41,
              sourceEndpoint: { localPath: source },
              destinationEndpoint: { localPath: destination },
            })),
            hasMore: false,
          },
        },
      }),
    );
    await page.goto("/exceptions?type=local");
    const input = page.getByRole("combobox", { name: "所属任务", exact: true });
    await input.click();
    const option = page.getByRole("option", { name: /^任务 #41 / });
    await expect(option).toBeVisible();
    const list = page.getByRole("listbox");
    const menu = list.locator("..");
    const menuStyle = await menu.evaluate((element) => {
      const style = getComputedStyle(element);
      const rect = element.getBoundingClientRect();
      return {
        shadow: style.boxShadow,
        border: parseFloat(style.borderTopWidth),
        left: rect.left,
        right: rect.right,
      };
    });
    expect(menuStyle.shadow).not.toBe("none");
    expect(menuStyle.border).toBeGreaterThan(0);
    expect(menuStyle.left).toBeGreaterThanOrEqual(0);
    expect(menuStyle.right).toBeLessThanOrEqual(width);
    expect((await option.boundingBox())!.height).toBeLessThan(72);
    await expect(option.getByTitle(`${source} → ${destination}`)).toHaveCSS(
      "text-overflow",
      "ellipsis",
    );
    expect(
      await list.evaluate(
        (element) => element.scrollHeight > element.clientHeight,
      ),
    ).toBe(true);
    await option.click();
    await expect(input).toHaveValue("41");
    await expect(list).toBeHidden();
    await expect
      .poll(() =>
        requests.some(
          (request) =>
            request.path.startsWith("/api/master/local-transfers/failures?") &&
            new URL(request.path, "http://localhost").searchParams.get(
              "taskId",
            ) === "41",
        ),
      )
      .toBe(true);
  });
}

for (const mode of ["failed", "local"] as const) {
  test(`${mode} retry success keeps the original exception and completed task choice until explicit cleanup`, async ({
    page,
  }) => {
    await mockApi(page);
    let completed = false;
    let cleared = false;
    let clearRequests = 0;
    const rows = () =>
      cleared
        ? []
        : [
            {
              ...row(61, completed ? "RECOVERED" : "NOT_RETRIED"),
              direction: mode === "local" ? "OSS_TO_LOCAL" : "OSS_TO_OSS",
              errorMessage: "Original upload failure",
              retryCount: completed ? 1 : 0,
            },
          ];
    await page.route("**/api/master/tasks/61", (route) =>
      route.fulfill({
        json: {
          success: true,
          data: {
            taskIndex: 61,
            status: completed ? "COMPLETED" : "PARTIAL_FAILED",
            generation: completed ? 2 : 1,
          },
        },
      }),
    );
    await page.route("**/api/master/exceptions/tasks?**", (route) =>
      route.fulfill({
        json: {
          success: true,
          data: {
            items: cleared
              ? []
              : [
                  {
                    taskIndex: 61,
                    status: completed ? "COMPLETED" : "PARTIAL_FAILED",
                  },
                ],
            hasMore: false,
          },
        },
      }),
    );
    if (mode === "local") {
      await page.route("**/api/master/local-transfers/failures?**", (route) =>
        route.fulfill({
          json: {
            success: true,
            data: {
              items: rows(),
              total: rows().length,
              clearableCount: completed && !cleared ? 1 : 0,
            },
          },
        }),
      );
    } else {
      await page.route("**/api/master/failed", (route) =>
        route.fulfill({
          json: {
            success: true,
            failedKeys: {
              "10.0.1.12": rows(),
              "10.0.1.12Count": rows().length,
            },
            clearableCount: completed && !cleared ? 1 : 0,
          },
        }),
      );
    }
    await page.route("**/api/master/retry", (route) => {
      expect(route.request().postDataJSON()).toEqual({
        taskIds: [61],
        expectedGenerations: { "61": 1 },
      });
      completed = true;
      return route.fulfill({
        json: {
          accepted: 1,
          ignored: 0,
          failed: 0,
          details: [
            { taskId: 61, generation: 2, status: "RESUMING", success: true },
          ],
        },
      });
    });
    await page.route("**/api/master/failed/clear", (route) => {
      clearRequests++;
      expect(route.request().postDataJSON()).toEqual({
        type: mode,
        taskIndex: 61,
      });
      cleared = true;
      return route.fulfill({
        json: { success: true, complete: true, clearedCount: 1 },
      });
    });
    await page.goto(`/exceptions?taskId=61&type=${mode}`);
    const table = page.getByRole("table");
    const cleanup = page.getByRole("button", {
      name: "清理已处理记录",
      exact: true,
    });
    await expect(table).toContainText("Original upload failure");
    await expect(cleanup).toBeDisabled();
    await page
      .getByRole("region", { name: "任务操作", exact: true })
      .getByRole("button", { name: "重试失败项", exact: true })
      .click();
    await expect(table).toContainText("已恢复");
    await expect(table).toContainText("file-61.bin");
    await expect(table).toContainText("Original upload failure");
    await expect(cleanup).toBeEnabled();
    expect(clearRequests).toBe(0);
    await page.reload();
    await expect(table).toContainText("已恢复");
    await expect(
      page.getByRole("button", { name: "任务已完成", exact: true }),
    ).toBeDisabled();
    const taskFilter = page.getByRole("combobox", {
      name: "所属任务",
      exact: true,
    });
    await taskFilter.click();
    await expect(
      page.getByRole("option", { name: "任务 #61", exact: true }),
    ).toBeVisible();
    await taskFilter.press("Escape");
    await cleanup.click();
    await page.getByRole("button", { name: "确认清理", exact: true }).click();
    await expect(table).toHaveCount(0);
    await expect(cleanup).toBeDisabled();
    expect(clearRequests).toBe(1);
    await taskFilter.click();
    await expect(
      page.getByRole("option", { name: "全部任务", exact: true }),
    ).toBeVisible();
    await expect(
      page.getByRole("option", { name: "任务 #61", exact: true }),
    ).toHaveCount(0);
  });
}
