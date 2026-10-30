import { expect, test, type Page } from "@playwright/test";
import { mockApi, tasks } from "./fixtures";
import { mockTaskChildren, revealChildren } from "./task-children-fixture";

async function withoutSignalHelpers(page: Page) {
  await page.addInitScript(() => {
    for (const name of ["any", "timeout"])
      Object.defineProperty(AbortSignal, name, {
        configurable: true,
        value: undefined,
      });
    Object.defineProperty(AbortSignal.prototype, "throwIfAborted", {
      configurable: true,
      value: undefined,
    });
    localStorage.setItem("oss-transfer-refresh-seconds", "0");
  });
  await mockApi(page);
}

test("tasks, lazy children and the task catalog work without newer AbortSignal methods", async ({
  page,
}) => {
  await withoutSignalHelpers(page);
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.route("**/api/master/status", (route) =>
    route.fulfill({
      json: {
        tasks: tasks.map((task) =>
          task.taskIndex === 1048
            ? { ...task, ossTaskGroup: true, shardCount: 1 }
            : task,
        ),
      },
    }),
  );
  await mockTaskChildren(page, () => ({
    parentTaskId: 1048,
    items: [
      {
        ...tasks[0],
        parentTaskId: 1048,
        subtaskNumber: 1,
        childTaskId: "1048-1",
        shardId: "1048-1",
      },
    ],
  }));
  await page.goto("/tasks");
  await page.getByRole("button", { name: "#1048", exact: true }).click();
  await revealChildren(page);
  await expect(
    page.getByRole("button", { name: "查看子任务 1048-1", exact: true }),
  ).toBeVisible();
  const ids = await page.evaluate(async () => {
    const module = "/src/lib/taskCatalog.ts";
    const { loadTaskCatalog } = await import(module);
    return (await loadTaskCatalog(new AbortController().signal)).map(
      (task: { taskIndex: number }) => task.taskIndex,
    );
  });
  expect(ids).toEqual(tasks.map((task) => task.taskIndex));
  expect(errors).toEqual([]);
});

test("network timeout and caller cancellation remain distinct without native signal helpers", async ({
  page,
}) => {
  await withoutSignalHelpers(page);
  const requested: string[] = [];
  let release!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route("**/api/compat/hold?*", async (route) => {
    requested.push(new URL(route.request().url()).searchParams.get("case")!);
    await held;
    await route.fulfill({ json: { success: true } }).catch(() => {});
  });
  await page.goto("/tasks");
  const result = await page.evaluate(async () => {
    const module = "/src/lib/api.tsx";
    const { api, ApiError } = await import(module);
    const capture = async (request: Promise<unknown>) => {
      try {
        await request;
        return { apiError: false, name: "resolved", message: "" };
      } catch (error) {
        return {
          apiError: error instanceof ApiError,
          name: (error as Error).name,
          message: (error as Error).message,
        };
      }
    };
    const timeout = await capture(
      api("/api/compat/hold?case=timeout", { timeoutMs: 500 }),
    );
    const caller = new AbortController();
    Object.defineProperty(caller.signal, "reason", { value: undefined });
    const pending = capture(
      api("/api/compat/hold?case=cancel", {
        signal: caller.signal,
        timeoutMs: 9137,
      }),
    );
    setTimeout(() => caller.abort(), 500);
    const cancel = await pending;
    const already = new AbortController();
    Object.defineProperty(already.signal, "reason", { value: undefined });
    already.abort();
    const preAborted = await capture(
      api("/api/compat/hold?case=already", { signal: already.signal }),
    );
    return { timeout, cancel, preAborted };
  });
  release();
  expect(result.timeout.apiError).toBe(true);
  expect(result.timeout.message).toContain("请求超时");
  expect(result.cancel).toMatchObject({ apiError: false, name: "AbortError" });
  expect(result.preAborted).toMatchObject({
    apiError: false,
    name: "AbortError",
  });
  expect(requested).toEqual(["timeout", "cancel"]);
});

test("timeouts cover response bodies and completed requests release timers and listeners", async ({
  page,
}) => {
  await withoutSignalHelpers(page);
  await page.goto("/tasks");
  const result = await page.evaluate(async () => {
    const module = "/src/lib/api.tsx";
    const { api, ApiError } = await import(module);
    const originalFetch = window.fetch;
    const originalSetTimeout = window.setTimeout;
    const originalClearTimeout = window.clearTimeout;
    const timers = new Set<number>();
    window.setTimeout = ((
      handler: TimerHandler,
      delay?: number,
      ...args: unknown[]
    ) => {
      const id = originalSetTimeout(handler, delay, ...args);
      if (delay === 9137) timers.add(id);
      return id;
    }) as typeof window.setTimeout;
    window.clearTimeout = (id?: number) => {
      if (id != null) timers.delete(id);
      originalClearTimeout(id);
    };
    const parent = new AbortController();
    const listeners = new Set<EventListenerOrEventListenerObject>();
    const add = parent.signal.addEventListener.bind(parent.signal);
    const remove = parent.signal.removeEventListener.bind(parent.signal);
    parent.signal.addEventListener = ((
      type: string,
      listener: EventListenerOrEventListenerObject,
      options?: AddEventListenerOptions | boolean,
    ) => {
      if (type === "abort") listeners.add(listener);
      add(type, listener, options);
    }) as typeof parent.signal.addEventListener;
    parent.signal.removeEventListener = ((
      type: string,
      listener: EventListenerOrEventListenerObject,
      options?: EventListenerOptions | boolean,
    ) => {
      if (type === "abort") listeners.delete(listener);
      remove(type, listener, options);
    }) as typeof parent.signal.removeEventListener;
    let bodyHeadersReceived = false;
    window.fetch = async (input, options) => {
      if (String(input) === "/api/compat/success")
        return new Response('{"success":true}', {
          headers: { "Content-Type": "application/json" },
        });
      if (String(input) === "/api/compat/body") {
        bodyHeadersReceived = true;
        return new Response(
          new ReadableStream({
            start(controller) {
              controller.enqueue(new TextEncoder().encode('{"success":'));
              options!.signal!.addEventListener(
                "abort",
                () => {
                  controller.error(
                    new DOMException("Body aborted", "AbortError"),
                  );
                },
                { once: true },
              );
            },
          }),
          { headers: { "Content-Type": "application/json" } },
        );
      }
      return originalFetch(input, options);
    };
    try {
      // A long-lived parent is reused by polling. No completed request should
      // leave a listener or a pending deadline attached to it.
      for (let index = 0; index < 3; index++)
        await api("/api/compat/success", {
          signal: parent.signal,
          timeoutMs: 9137,
        });
      const afterSuccess = { timers: timers.size, listeners: listeners.size };
      let bodyTimeout = false;
      try {
        await api("/api/compat/body", { timeoutMs: 30 });
      } catch (error) {
        bodyTimeout =
          error instanceof ApiError &&
          (error as Error).message.includes("请求超时");
      }
      const pending = api("/api/compat/body", {
        signal: parent.signal,
        timeoutMs: 9137,
      });
      parent.abort();
      let cancelled = false;
      try {
        await pending;
      } catch (error) {
        cancelled = (error as Error).name === "AbortError";
      }
      return {
        afterSuccess,
        bodyHeadersReceived,
        bodyTimeout,
        cancelled,
        afterCancel: { timers: timers.size, listeners: listeners.size },
      };
    } finally {
      window.fetch = originalFetch;
      window.setTimeout = originalSetTimeout;
      window.clearTimeout = originalClearTimeout;
    }
  });
  expect(result.afterSuccess).toEqual({ timers: 0, listeners: 0 });
  expect(result.bodyHeadersReceived).toBe(true);
  expect(result.bodyTimeout).toBe(true);
  expect(result.cancelled).toBe(true);
  expect(result.afterCancel).toEqual({ timers: 0, listeners: 0 });
});
