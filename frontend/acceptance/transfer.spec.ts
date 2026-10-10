import { expect, test, type Page, type Request } from "@playwright/test";
import { writeFile } from "node:fs/promises";
import { isIP } from "node:net";
import { isAbsolute } from "node:path";
import { setTimeout as delay } from "node:timers/promises";

function required(name: string) {
  const value = process.env[name];
  if (!value) throw new Error(`Missing ${name}`);
  return value;
}

async function select(page: Page, label: string, option: string) {
  await page.getByRole("combobox", { name: label }).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}

async function observeCompletion(
  page: Page,
  origin: string,
  creationId: string,
  authorization: string,
) {
  const deadline = performance.now() + 90_000;
  let taskId: number | undefined;
  async function read(path: string) {
    const response = await page.request.get(new URL(path, origin).toString(), {
      headers: { Authorization: authorization },
      timeout: 10_000,
      maxRedirects: 0,
    });
    if (!response.ok())
      throw new Error(
        `Completion observation failed: HTTP ${response.status()}`,
      );
    return response.json();
  }
  while (performance.now() < deadline) {
    if (taskId === undefined) {
      const result = await read(
        `/api/master/task-creations/${encodeURIComponent(creationId)}`,
      );
      const creation = result.creation;
      if (result.success !== true || creation?.id !== creationId)
        throw new Error(
          "Completion observation received an invalid creation record",
        );
      if (creation.status === "FAILED")
        throw new Error("OSS migration creation failed");
      if (
        Number.isSafeInteger(creation.publicTaskId) &&
        creation.publicTaskId > 0
      )
        taskId = creation.publicTaskId;
    }
    if (taskId !== undefined) {
      const snapshot = await read("/api/master/status?includeSubtasks=false");
      if (!Array.isArray(snapshot.tasks))
        throw new Error(
          "Completion observation received an invalid task snapshot",
        );
      const task = snapshot.tasks.find(
        (candidate: { taskIndex: number }) => candidate.taskIndex === taskId,
      );
      if (
        ["FAILED", "PARTIAL_FAILED", "STOPPED", "STOPPING"].includes(
          task?.status,
        )
      )
        throw new Error(`OSS migration ended unexpectedly: ${task.status}`);
      if (task?.status === "COMPLETED") {
        const completedAtUnixMs = Date.now();
        if (task.snapshotComplete === false || snapshot.partial === true)
          throw new Error("Completed task snapshot is incomplete");
        return { taskId, completedAtUnixMs };
      }
    }
    await delay(250);
  }
  throw new Error("OSS migration did not complete within 90 seconds");
}

test("packaged console logs in and creates a real OSS migration", async ({
  page,
}) => {
  const origin = new URL(required("ACCEPTANCE_BASE_URL"));
  if (
    !["http:", "https:"].includes(origin.protocol) ||
    origin.pathname !== "/" ||
    origin.search ||
    origin.hash ||
    origin.username ||
    origin.password
  )
    throw new Error("ACCEPTANCE_BASE_URL must be the master HTTP origin");
  const password = required("ACCEPTANCE_PASSWORD");
  const alias = required("ACCEPTANCE_S3_ALIAS");
  const source = required("ACCEPTANCE_SOURCE_BUCKET");
  const target = required("ACCEPTANCE_TARGET_BUCKET");
  const workers: unknown = JSON.parse(required("ACCEPTANCE_WORKERS"));
  if (
    !Array.isArray(workers) ||
    workers.length !== 2 ||
    new Set(workers).size !== 2 ||
    !workers.every((worker) => typeof worker === "string" && isIP(worker) === 4)
  )
    throw new Error(
      "ACCEPTANCE_WORKERS must contain two distinct Agent IPv4 addresses",
    );
  const resultPath = required("ACCEPTANCE_RESULT");
  if (!isAbsolute(resultPath))
    throw new Error("ACCEPTANCE_RESULT must be an absolute output path");
  if (source === target) throw new Error("Acceptance buckets must be distinct");

  const creations: Request[] = [];
  let startedAtUnixMs: number | undefined;
  page.on("request", (request) => {
    if (
      request.method() === "POST" &&
      new URL(request.url()).pathname === "/api/master/add-buckets"
    ) {
      startedAtUnixMs ??= Date.now();
      creations.push(request);
    }
  });

  await page.goto("/");
  await page.getByLabel(/^用户名/).fill("admin");
  await page.getByLabel(/^密码/).fill(password);
  await page.getByRole("button", { name: "登录", exact: true }).click();
  await expect(page.getByRole("heading", { name: "迁移概览" })).toBeVisible();
  await page.goto("/create");
  await select(page, "源存储连接", alias);
  await page
    .getByRole("combobox", { name: "源存储桶", exact: true })
    .fill(source);
  await select(page, "目标存储连接", alias);
  await page
    .getByRole("combobox", { name: "目标存储桶", exact: true })
    .fill(target);
  await select(page, "目标对象命名", "添加目标前缀");
  await page
    .getByRole("textbox", { name: "目标对象前缀", exact: true })
    .fill("browser/");
  await page.getByRole("button", { name: /迁移选项/ }).click();
  await select(page, "目标已存在时", "覆盖同名对象或文件");
  await select(page, "节点分发方式", "均衡分发");
  await page
    .getByRole("spinbutton", { name: "任务带宽上限", exact: true })
    .fill("8");

  await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
  for (const worker of workers)
    await expect(
      page.getByRole("option", { name: worker, exact: true }),
    ).toBeEnabled();
  const options = page.getByRole("option");
  for (let index = 0, count = await options.count(); index < count; index++) {
    const option = options.nth(index);
    const address = (await option.innerText()).trim().split(/\s/)[0];
    const selected = workers.includes(address);
    if ((await option.getAttribute("aria-selected")) !== String(selected))
      await option.click();
    await expect(option).toHaveAttribute("aria-selected", String(selected));
  }
  await page.keyboard.press("Escape");

  const submitted = page
    .waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === "/api/master/add-buckets",
    )
    .then(async (response) => {
      expect(response.ok()).toBe(true);
      const accepted = await response.json();
      expect(accepted.success).toBe(true);
      expect(accepted.addedCount).toBe(1);
      expect(accepted.creationIds).toHaveLength(1);
      const creationId: unknown = accepted.creationIds[0];
      if (typeof creationId !== "string" || !creationId)
        throw new Error("Creation did not return its record ID");
      const authorization = (await response.request().allHeaders())
        .authorization;
      if (!authorization)
        throw new Error("Creation request was not authenticated");
      return { creationId, authorization };
    });
  // Start observing as soon as the creation response arrives, independently of UI checks.
  const completed = submitted.then(({ creationId, authorization }) =>
    observeCompletion(page, origin.toString(), creationId, authorization),
  );
  const inspectCreatedTask = async () => {
    await page
      .getByRole("button", { name: "创建迁移任务", exact: true })
      .click();
    const { creationId } = await submitted;
    const created = page.waitForResponse(async (candidate) => {
      if (
        !candidate.ok() ||
        new URL(candidate.url()).pathname !==
          `/api/master/task-creations/${encodeURIComponent(creationId)}`
      )
        return false;
      const data = await candidate.json();
      return (
        Number.isSafeInteger(data.creation?.publicTaskId) &&
        data.creation.publicTaskId > 0
      );
    });
    await page.getByRole("link", { name: "查看任务", exact: true }).click();
    const creation = (await (await created).json()).creation;
    expect(creation.id).toBe(creationId);
    expect(creation.status).not.toBe("FAILED");
    const taskId: number = creation.publicTaskId;
    await expect(page).toHaveURL(
      (url) =>
        url.pathname === "/tasks" &&
        url.searchParams.get("creationId") === creationId &&
        url.searchParams.get("taskId") === String(taskId),
    );
    await expect(
      page.getByRole("heading", { name: `迁移任务 #${taskId}`, exact: true }),
    ).toBeVisible();
    return taskId;
  };
  const [completion, taskId] = await Promise.all([
    completed,
    inspectCreatedTask(),
  ]);
  expect(completion.taskId).toBe(taskId);

  expect(creations).toHaveLength(1);
  const payload = creations[0].postDataJSON();
  expect(payload).toHaveLength(1);
  expect(payload[0]).toMatchObject({
    sourceS3Alias: alias,
    sourceBucket: source,
    destS3Alias: alias,
    destBucket: target,
    overwrite: true,
    distributionMode: "balanced",
    taskBandwidthLimitMbps: 8,
    keyMapping: { mode: "ADD_PREFIX", targetPrefix: "browser/" },
  });
  expect([...payload[0].eligibleWorkerNodes].sort()).toEqual(
    [...workers].sort(),
  );

  // The runner verifies object bytes; timing ends at the first observed completion.
  expect(startedAtUnixMs).toBeDefined();
  await writeFile(
    resultPath,
    JSON.stringify({
      taskId,
      startedAtUnixMs,
      completedAtUnixMs: completion.completedAtUnixMs,
      creationRequestCount: creations.length,
    }) + "\n",
    {
      mode: 0o600,
      flag: "wx",
    },
  );
});
