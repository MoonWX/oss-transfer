import type { Page, Request, Route } from "@playwright/test";

export type Row = {
  number: number;
  key: string;
  sourceKey: string;
  destinationKey: string;
  status: string;
  sourceSize: number;
  destinationSize: number;
  sourceSha256?: string;
  destinationSha256?: string;
  error?: string;
  errorDetail?: string;
  reasons?: string[];
};

export const progress = {
  id: "comparison-1",
  storageRevision: 3,
  status: "COMPLETED",
  mode: "metadata",
  scope: "prefix",
  source: { kind: "OSS", ref: "source", bucket: "photos" },
  destination: { kind: "OSS", ref: "target", bucket: "archive" },
  sourcePrefix: "2026/",
  destinationPrefix: "backup/",
  compared: 10,
  matched: 6,
  different: 1,
  etagMismatch: 1,
  missing: 1,
  extra: 1,
  changed: 0,
  errors: 0,
  bothMissing: 0,
  rows: 4,
  rowsTruncated: false,
  resultLimit: 0,
  storedBytes: 1200,
  bytesRead: 0,
  elapsedMillis: 4000,
  resumable: false,
  resultsProtocol: 2,
  startedAt: "2026-09-22T11:00:00Z",
  updatedAt: "2026-09-22T11:00:04Z",
  endedAt: "2026-09-22T11:00:04Z",
};

export function row(
  number: number,
  status: string,
  extra: Partial<Row> = {},
): Row {
  const key = `file-${String(number).padStart(4, "0")}.bin`;
  return {
    number,
    key,
    sourceKey: `2026/${key}`,
    destinationKey: `backup/${key}`,
    status,
    sourceSize: 4,
    destinationSize: 8,
    ...extra,
  };
}

export const rows = [
  row(1, "DIFFERENT", { reasons: ["size", "contentType", "metadata"] }),
  row(2, "ETAG_MISMATCH", { reasons: ["etag"], destinationSize: 4 }),
  row(3, "MISSING_TARGET", { destinationSize: 0 }),
  row(4, "EXTRA_TARGET", { sourceSize: 0 }),
];

/**
 * Emulates the Master's page: rows after `after`, filtered by `status`,
 * at most `limit`, with nextAfter set while stored rows remain.
 */
export function pageOf(current: object, stored: Row[], query: URLSearchParams) {
  const after = Number(query.get("after") || 0);
  const limit = Number(query.get("limit") || 50);
  const statuses = query.get("status")?.split(",") ?? [];
  const items = stored
    .filter(
      (item) =>
        item.number > after &&
        (statuses.length === 0 || statuses.includes(item.status)),
    )
    .slice(0, limit);
  const last = items.at(-1)?.number ?? after;
  const nextAfter =
    items.length === limit && stored.some((item) => item.number > last)
      ? last
      : 0;
  return { ...current, items, nextAfter };
}

export type Reply = { status?: number; json?: unknown };
export type ComparisonApi = {
  list?: () => Reply | unknown;
  start?: (body: unknown) => Reply | unknown;
  page?: (id: string, query: URLSearchParams) => Reply | unknown;
  cancel?: (id: string) => Reply | unknown;
  resume?: (id: string) => Reply | unknown;
  remove?: (id: string) => Reply | unknown;
  ticket?: (id: string, query: URLSearchParams) => Reply | unknown;
  /** Handles the ticketed POST itself (attachments need custom headers). */
  download?: (route: Route, id: string) => Promise<void>;
};
export type Call = {
  kind: keyof ComparisonApi;
  id?: string;
  query: URLSearchParams;
  request: Request;
};

/**
 * Requests the page did not abort. React StrictMode mounts effects twice in
 * development, so the first request of every effect is canceled at once.
 */
export const live = (calls: Call[], kind?: keyof ComparisonApi) =>
  calls.filter(
    (call) => (!kind || call.kind === kind) && call.request.failure() === null,
  );

/** {status: number, json} is an HTTP reply; anything else is a JSON body. */
const isReply = (value: unknown): value is Reply =>
  !!value &&
  typeof value === "object" &&
  "json" in value &&
  Object.keys(value).every((key) => key === "status" || key === "json") &&
  ["number", "undefined"].includes(typeof (value as Reply).status);

/** Routes every /api/master/comparisons request to the given handlers. */
export async function routeComparisons(page: Page, api: ComparisonApi) {
  const calls: Call[] = [];
  await page.route(
    (url) => url.pathname.startsWith("/api/master/comparisons"),
    async (route) => {
      const request = route.request();
      const url = new URL(request.url());
      const parts = url.pathname.split("/").slice(4).map(decodeURIComponent);
      const [id, action] = parts;
      const method = request.method();
      let kind: keyof ComparisonApi | undefined;
      if (!id) kind = method === "POST" ? "start" : "list";
      else if (!action) kind = method === "DELETE" ? "remove" : "page";
      else if (action === "cancel") kind = "cancel";
      else if (action === "resume") kind = "resume";
      else if (action === "export-ticket") kind = "ticket";
      else if (action === "export") kind = "download";
      if (!kind) return route.fulfill({ status: 404, json: {} });
      calls.push({ kind, id, query: url.searchParams, request });
      if (kind === "download") {
        if (api.download) return api.download(route, id);
        return route.fulfill({
          status: 404,
          json: { success: false, message: "Not found" },
        });
      }
      const handler = api[kind] as
        ((...args: unknown[]) => Reply | unknown) | undefined;
      if (!handler)
        return route.fulfill({
          status: 404,
          json: { success: false, message: "Comparison not found" },
        });
      const result =
        kind === "list"
          ? handler()
          : kind === "start"
            ? handler(request.postDataJSON())
            : handler(id, url.searchParams);
      const reply = isReply(result) ? result : { json: result };
      return route.fulfill({ status: reply.status ?? 200, json: reply.json });
    },
  );
  return calls;
}

export const historyItem = (
  extra: Record<string, unknown> = {},
): Record<string, unknown> => ({
  ...progress,
  nodeId: "node-a",
  nodeName: "agent-a",
  ...extra,
});
