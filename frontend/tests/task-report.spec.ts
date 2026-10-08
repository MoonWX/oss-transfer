import { expect, test, type Page, type Request } from "@playwright/test";
import { readFile } from "node:fs/promises";
import { mockApi, tasks } from "./fixtures";

const zip = Buffer.from("UEsFBgAAAAAAAAAAAAAAAAAAAAAAAA==", "base64");
const reportTasks = tasks.map((task) =>
  task.taskIndex === 1045
    ? { ...task, direction: "LOCAL_TO_OSS", localPath: "/mnt/upload" }
    : { ...task, status: task.taskIndex === 1048 ? "COMPLETED" : task.status },
);

async function prepareReports(page: Page) {
  await mockApi(page);
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: { totalTasks: reportTasks.length, tasks: reportTasks },
    }),
  );
  await page.addInitScript(() => {
    Object.assign(window, { reportBlobReads: 0, reportObjectUrls: 0 });
    const blob = Response.prototype.blob;
    Response.prototype.blob = function () {
      Reflect.set(
        window,
        "reportBlobReads",
        Number(Reflect.get(window, "reportBlobReads")) + 1,
      );
      return blob.call(this);
    };
    const objectUrl = URL.createObjectURL;
    URL.createObjectURL = function (value) {
      Reflect.set(
        window,
        "reportObjectUrls",
        Number(Reflect.get(window, "reportObjectUrls")) + 1,
      );
      return objectUrl.call(this, value);
    };
  });
  await page.goto("/tasks");
}

async function openReport(page: Page, taskId: number) {
  await page
    .getByRole("button", {
      name: `#${taskId}`,
      exact: true,
    })
    .click();
  await expect(
    page.getByRole("heading", { name: `迁移任务 #${taskId}` }),
  ).toBeVisible();
  await expect(
    page.getByText(
      "仅导出当前任务，包含已记录的成功、跳过、失败和未处理对象。",
    ),
  ).toHaveCount(0);
  await expect(page.getByText(/历史任务可能缺少对象明细/)).toHaveCount(0);
}

test("all three transfer directions download task reports using authenticated tickets and native POST attachments", async ({
  page,
}, testInfo) => {
  await prepareReports(page);
  const tickets: Request[] = [];
  const posts: Request[] = [];
  const navigationRequests: Request[] = [];
  const popups: Page[] = [];
  page.on("popup", (popup) => popups.push(popup));
  page.on("request", (request) => {
    if (request.isNavigationRequest() && request.frame() === page.mainFrame())
      navigationRequests.push(request);
  });
  await page.route(
    "**/api/master/task-reports/*/download-ticket",
    async (route) => {
      tickets.push(route.request());
      const id = new URL(route.request().url()).pathname.split("/").at(-2);
      await route.fulfill({
        json: { success: true, data: { ticket: `ticket-${id}` } },
      });
    },
  );
  await page.route("**/api/master/task-reports/*/download", async (route) => {
    posts.push(route.request());
    const id = new URL(route.request().url()).pathname.split("/").at(-2);
    await route.fulfill({
      contentType: "application/zip",
      headers: {
        "Content-Disposition": `attachment; filename="task-${id}-report.zip"`,
      },
      body: zip,
    });
  });
  const shell = await page
    .getByRole("button", { name: "账号设置" })
    .elementHandle();
  for (const id of [1048, 1046, 1045]) {
    if (id === 1045) await page.setViewportSize({ width: 360, height: 800 });
    await openReport(page, id);
    if (id === 1045) {
      const notice = page
        .getByRole("alert")
        .filter({ hasText: "报告下载已请求" });
      if (await notice.isVisible()) await notice.getByRole("button").click();
      await expect(notice).not.toBeVisible();
      await page
        .getByRole("button", { name: "导出任务报告", exact: true })
        .scrollIntoViewIfNeeded();
      expect(
        await page.evaluate(() => document.documentElement.scrollWidth),
      ).toBeLessThanOrEqual(360);
      await page.screenshot({
        path: testInfo.outputPath("task-report-mobile.png"),
      });
    }
    const pendingDownload = page.waitForEvent("download");
    await page
      .getByRole("button", { name: "导出任务报告", exact: true })
      .click();
    const download = await pendingDownload;
    expect(download.suggestedFilename()).toBe(`task-${id}-report.zip`);
    const saved = testInfo.outputPath(`task-${id}-report.zip`);
    await download.saveAs(saved);
    expect(await readFile(saved)).toEqual(zip);
    await page.getByRole("button", { name: "关闭详情" }).click();
  }
  expect(tickets).toHaveLength(3);
  expect(posts).toHaveLength(3);
  for (const request of tickets) {
    expect(request.method()).toBe("GET");
    expect(request.headers().authorization).toBe("Bearer test-token");
    expect(new URL(request.url()).search).toBe("");
  }
  for (const request of posts) {
    const id = new URL(request.url()).pathname.split("/").at(-2);
    expect(request.method()).toBe("POST");
    expect(request.resourceType()).toBe("document");
    expect(request.headers().authorization).toBeUndefined();
    expect(new URL(request.url()).search).toBe("");
    expect([...new URLSearchParams(request.postData() || "")]).toEqual([
      ["ticket", `ticket-${id}`],
    ]);
  }
  expect(
    await page.evaluate(() => [
      Reflect.get(window, "reportBlobReads"),
      Reflect.get(window, "reportObjectUrls"),
    ]),
  ).toEqual([0, 0]);
  await expect(page.locator('form[action$="/download"]')).toHaveCount(0);
  const frames = page.locator("iframe[data-task-report-download]");
  await expect(frames).toHaveCount(3);
  for (const frame of await frames.all()) await expect(frame).toBeHidden();
  expect(await shell!.evaluate((element) => element.isConnected)).toBe(true);
  expect(navigationRequests).toHaveLength(0);
  expect(popups).toHaveLength(0);
  await expect(page).toHaveURL(/\/tasks$/);
});

for (const direction of ["OSS_TO_OSS", "LOCAL_TO_OSS", "OSS_TO_LOCAL"]) {
  test(`stopped ${direction} task can download its snapshot report`, async ({
    page,
  }, testInfo) => {
    await mockApi(page);
    await page.route("**/api/master/status", (route) =>
      route.fulfill({
        json: {
          totalTasks: 1,
          tasks: [{ ...tasks[0], status: "STOPPED", direction }],
        },
      }),
    );
    await page.route(
      "**/api/master/task-reports/1048/download-ticket",
      (route) =>
        route.fulfill({
          json: { success: true, data: { ticket: "stopped-snapshot" } },
        }),
    );
    let posts = 0;
    await page.route("**/api/master/task-reports/1048/download", (route) => {
      posts++;
      expect(route.request().method()).toBe("POST");
      expect(route.request().postData()).toBe("ticket=stopped-snapshot");
      return route.fulfill({
        contentType: "application/zip",
        headers: {
          "Content-Disposition": 'attachment; filename="task-1048-report.zip"',
        },
        body: zip,
      });
    });
    await page.goto("/tasks?taskId=1048");
    const download = page.waitForEvent("download");
    await page
      .getByRole("button", { name: "导出任务报告", exact: true })
      .click();
    const report = await download;
    const saved = testInfo.outputPath("stopped-report.zip");
    await report.saveAs(saved);
    expect(await readFile(saved)).toEqual(zip);
    expect(posts).toBe(1);
    await expect(
      page.getByText("停止或结束后可导出报告。", { exact: true }),
    ).toHaveCount(0);
  });
}

test("starting a second report keeps an earlier pending download independent", async ({
  page,
}, testInfo) => {
  await prepareReports(page);
  let issued = 0;
  let releaseTicket!: () => void;
  const ticketGate = new Promise<void>((resolve) => {
    releaseTicket = resolve;
  });
  let firstStarted!: () => void;
  const firstRequest = new Promise<void>((resolve) => {
    firstStarted = resolve;
  });
  let releaseFirst!: () => void;
  const firstGate = new Promise<void>((resolve) => {
    releaseFirst = resolve;
  });
  await page.route(
    "**/api/master/task-reports/1048/download-ticket",
    async (route) => {
      const ticket = `ticket-${++issued}`;
      if (issued === 1) await ticketGate;
      await route.fulfill({ json: { success: true, data: { ticket } } });
    },
  );
  await page.route(
    "**/api/master/task-reports/1048/download",
    async (route) => {
      const first =
        new URLSearchParams(route.request().postData() || "").get("ticket") ===
        "ticket-1";
      if (first) {
        firstStarted();
        await firstGate;
      }
      await route.fulfill({
        contentType: "application/zip",
        headers: {
          "Content-Disposition": `attachment; filename="report-${first ? 1 : 2}.zip"`,
        },
        body: zip,
      });
    },
  );
  await openReport(page, 1048);
  await page.getByRole("button", { name: "导出任务报告", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "正在准备下载", exact: true }),
  ).toBeDisabled();
  await expect(page.locator("iframe[data-task-report-download]")).toHaveCount(
    0,
  );
  releaseTicket();
  await firstRequest;
  const secondDownload = page.waitForEvent("download");
  await page.getByRole("button", { name: "导出任务报告", exact: true }).click();
  const second = await secondDownload;
  expect(second.suggestedFilename()).toBe("report-2.zip");
  await second.saveAs(testInfo.outputPath("report-2.zip"));
  await page.getByRole("button", { name: "关闭详情" }).click();
  await page.getByRole("link", { name: "存储连接", exact: true }).click();
  const firstDownload = page.waitForEvent("download");
  releaseFirst();
  const first = await firstDownload;
  expect(first.suggestedFilename()).toBe("report-1.zip");
  await first.saveAs(testInfo.outputPath("report-1.zip"));
  expect(await first.failure()).toBeNull();
  expect(await second.failure()).toBeNull();
  const targets = await page
    .locator("iframe[data-task-report-download]")
    .evaluateAll((frames) =>
      frames.map((frame) => (frame as HTMLIFrameElement).name),
    );
  expect(new Set(targets).size).toBe(2);
  await expect(page).toHaveURL(/\/storage$/);
});

for (const status of [401, 404]) {
  test(`ticket HTTP ${status} shows an error and does not submit a download`, async ({
    page,
  }) => {
    await prepareReports(page);
    await page.route(
      "**/api/master/task-reports/1048/download-ticket",
      (route) =>
        route.fulfill({
          status,
          json: {
            success: false,
            message: status === 401 ? "登录已过期" : "任务不存在",
          },
        }),
    );
    await openReport(page, 1048);
    await page
      .getByRole("button", { name: "导出任务报告", exact: true })
      .click();
    await expect(
      page.getByText(
        status === 401 ? "登录已过期，请重新登录。" : "任务不存在",
        { exact: true },
      ),
    ).toBeVisible();
    await expect(page.locator("iframe[data-task-report-download]")).toHaveCount(
      0,
    );
    if (status === 401) {
      await expect(
        page.getByRole("heading", { name: "登录控制台" }),
      ).toBeVisible();
      expect(
        await page.evaluate(() => sessionStorage.getItem("oss-transfer-token")),
      ).toBeNull();
    } else {
      await expect(page).toHaveURL(/\/tasks\?taskId=1048$/);
      await expect(
        page.getByRole("heading", { name: "迁移任务 #1048" }),
      ).toBeVisible();
    }
  });
}

test("an expired POST ticket reports the download error without replacing the page or expiring login", async ({
  page,
}) => {
  await prepareReports(page);
  await page.route("**/api/master/task-reports/1048/download-ticket", (route) =>
    route.fulfill({
      json: { success: true, data: { ticket: "expired-ticket" } },
    }),
  );
  await page.route("**/api/master/task-reports/1048/download", (route) =>
    route.fulfill({
      status: 401,
      json: { success: false, message: "下载授权已失效，请重新导出报告" },
    }),
  );
  await openReport(page, 1048);
  await page.getByRole("button", { name: "导出任务报告", exact: true }).click();
  await expect(
    page.getByText("任务 #1048：下载授权已失效，请重新导出报告", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(page.locator("iframe[data-task-report-download]")).toHaveCount(
    0,
  );
  await expect(
    page.getByRole("heading", { name: "迁移任务 #1048" }),
  ).toBeVisible();
  await expect(page).toHaveURL(/\/tasks\?taskId=1048$/);
  expect(
    await page.evaluate(() => sessionStorage.getItem("oss-transfer-token")),
  ).toBe("test-token");
});

test("download notice expires on its original deadline despite blur, hover and identical repeats", async ({
  page,
}) => {
  await prepareReports(page);
  await page.clock.install();
  await page.route("**/api/master/task-reports/1048/download-ticket", (route) =>
    route.fulfill({
      json: { success: true, data: { ticket: "qa-notice-ticket" } },
    }),
  );
  await page.route("**/api/master/task-reports/1048/download", (route) =>
    route.fulfill({
      contentType: "application/zip",
      headers: {
        "Content-Disposition": 'attachment; filename="task-1048-report.zip"',
      },
      body: zip,
    }),
  );
  await openReport(page, 1048);
  await page.clock.pauseAt(await page.evaluate(() => Date.now() + 1000));
  const download = page.waitForEvent("download");
  await page.getByRole("button", { name: "导出任务报告", exact: true }).click();
  await download;
  await page.clock.runFor(400);
  const notice = page
    .getByRole("alert", { includeHidden: true })
    .filter({ hasText: "报告下载已请求" });
  await expect(notice).toBeVisible();
  await notice.hover();
  await page.evaluate(() => window.dispatchEvent(new Event("blur")));
  await page.clock.runFor(4000);
  await expect(notice).toBeVisible();
  const secondDownload = page.waitForEvent("download");
  await page.getByRole("button", { name: "导出任务报告", exact: true }).click();
  await secondDownload;
  await notice.hover();
  await page.clock.runFor(3000);
  await expect(notice).toBeHidden();
});

test("a different download error replaces the notice and receives its own bounded deadline", async ({
  page,
}) => {
  await prepareReports(page);
  await page.clock.install();
  let message = "报告服务暂不可用";
  await page.route("**/api/master/task-reports/1048/download-ticket", (route) =>
    route.fulfill({ status: 503, json: { success: false, message } }),
  );
  await openReport(page, 1048);
  await page.clock.pauseAt(await page.evaluate(() => Date.now() + 1000));
  await page.getByRole("button", { name: "导出任务报告", exact: true }).click();
  await expect(page.getByText(message, { exact: true })).toBeVisible();
  await page.clock.runFor(8000);
  message = "节点暂不可达";
  await page.getByRole("button", { name: "导出任务报告", exact: true }).click();
  const notice = page
    .getByRole("alert", { includeHidden: true })
    .filter({ hasText: message });
  await expect(notice).toBeVisible();
  await expect(
    page.getByText("报告服务暂不可用", { exact: true }),
  ).toBeHidden();
  await page.clock.runFor(7000);
  await expect(notice).toBeVisible();
  await notice.hover();
  await page.evaluate(() => window.dispatchEvent(new Event("blur")));
  await page.clock.runFor(5500);
  await expect(notice).toBeHidden();
});
