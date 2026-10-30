import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type RefObject,
} from "react";
import { api } from "./api";
import {
  activeSubtaskCursor,
  allSubtaskCursor,
  compareActiveSubtasks,
  compareAllSubtasks,
} from "./subtasks";

export type ChildView = "all" | "active";
type Child = Record<string, any>;
type ChildPage = {
  parentTaskId: number;
  offset: number;
  items: Child[];
  totalCount: number;
  hasMore?: boolean;
  nextOffset?: number;
  partial?: boolean;
  legacyHistorical?: boolean;
  order?: "priority";
  cursor?: string;
  nextCursor?: string;
  updates?: Child[];
  detached?: boolean;
  indexing?: boolean;
};
type Cache = {
  pages: Map<number, ChildPage>;
  errors: Map<number, string>;
  inFlight: Map<number, AbortController>;
  indexing: boolean;
};
const identity = (child: Child) =>
  JSON.stringify([
    child.childTaskId ||
      child.subtaskNumber ||
      child.shardId ||
      child.taskIndex,
    child.executionNodeIp || child.workerNode,
  ]);

// Keep previously loaded rows while polling and fetch only the visible windows.
export function useTaskChildren({
  parentId,
  view,
  initialOffset,
  enabled,
  refreshMs,
  visibleOffsets,
  beforeRowsChange,
  visibleChildNumbers,
}: {
  parentId?: number;
  view: ChildView;
  initialOffset: number;
  enabled: boolean;
  refreshMs: number;
  visibleOffsets: RefObject<Set<number>>;
  beforeRowsChange?: () => void;
  visibleChildNumbers?: () => number[];
}) {
  const owner = useRef(parentId);
  const caches = useRef(new Map<string, Cache>());
  const [, redraw] = useState(0);
  if (owner.current !== parentId) {
    for (const cache of caches.current.values())
      for (const controller of cache.inFlight.values()) controller.abort();
    caches.current.clear();
    owner.current = parentId;
  }
  const key = `${view}:${initialOffset}`;
  if (!caches.current.has(key))
    caches.current.set(key, {
      pages: new Map(),
      errors: new Map(),
      inFlight: new Map(),
      indexing: false,
    });
  const cache = caches.current.get(key)!;
  const activity = useRef({ cache, enabled, epoch: 0 });
  if (activity.current.cache !== cache || activity.current.enabled !== enabled)
    activity.current = { cache, enabled, epoch: activity.current.epoch + 1 };
  const fetchPage = useCallback(
    async (offset: number, cursorOverride?: string) => {
      const epoch = activity.current.epoch;
      const isCurrent = () =>
        activity.current.cache === cache &&
        activity.current.enabled &&
        activity.current.epoch === epoch;
      if (!parentId || !isCurrent() || cache.inFlight.has(offset)) return;
      const controller = new AbortController();
      cache.inFlight.set(offset, controller);
      redraw((value) => value + 1);
      try {
        const cursor =
          cursorOverride ??
          cache.pages.get(offset)?.cursor ??
          [...cache.pages.values()].find((page) => page.nextOffset === offset)
            ?.nextCursor;
        const known =
          cache.pages.size > 0
            ? [
                ...new Set(
                  visibleChildNumbers?.() ??
                    cache.pages
                      .get(offset)
                      ?.items.map((child) => child.subtaskNumber) ??
                    [],
                ),
              ].slice(0, 200)
            : [];
        const result = await api<{ data: ChildPage }>(
          `/api/master/tasks/${parentId}/children?offset=${offset}&limit=25&view=${view}&order=priority${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}${known.length ? `&known=${known.join(",")}` : ""}`,
          {
            signal: controller.signal,
            timeoutMs: 120000,
          },
        );
        if (
          controller.signal.aborted ||
          owner.current !== parentId ||
          !isCurrent()
        )
          return;
        if (
          result.data.parentTaskId !== parentId ||
          result.data.offset !== offset
        )
          return;
        const page = result.data;
        cache.indexing = page.indexing === true;
        if (cache.indexing) {
          cache.errors.delete(offset);
          return;
        }
        beforeRowsChange?.();
        let retained: Child[] = [];
        if (page.order === "priority") {
          const updates = new Map(
            [...(page.updates ?? []), ...page.items].map((child) => [
              identity(child),
              child,
            ]),
          );
          // State changes move children between priority groups. Replace the
          // covered key range, rather than treating a child number as a rank.
          for (const [cachedOffset, cached] of cache.pages) {
            const items = cached.items.flatMap((child) => {
              const current = updates.get(identity(child)) ?? child;
              if (
                view === "active" &&
                ["COMPLETED", "FAILED", "PARTIAL_FAILED", "DELETED"].includes(
                  current.status,
                )
              )
                return [];
              const key =
                view === "active"
                  ? activeSubtaskCursor(current)
                  : allSubtaskCursor(current);
              return key <= (page.cursor ?? "") ||
                (page.hasMore !== false && key > (page.nextCursor ?? ""))
                ? [current]
                : [];
            });
            if (cachedOffset === offset) {
              retained = items;
              continue;
            }
            const covered =
              (cached.cursor ?? "") >= (page.cursor ?? "") &&
              (page.hasMore === false ||
                (cached.nextCursor ?? "") <= (page.nextCursor ?? ""));
            if (covered && items.length === 0) {
              cache.pages.delete(cachedOffset);
              cache.errors.delete(cachedOffset);
              continue;
            }
            cache.pages.set(cachedOffset, {
              ...cached,
              items,
              detached: cached.detached || covered,
            });
          }
        } else if (view === "active" && !page.legacyHistorical) {
          // A new active page may span completed rows formerly held by later
          // windows. Retire that covered range before merging its live rows.
          const through =
            page.hasMore === false
              ? Infinity
              : (page.nextOffset ?? offset + 25);
          for (const failedOffset of cache.errors.keys())
            if (failedOffset > offset && failedOffset < through)
              cache.errors.delete(failedOffset);
          for (const [cachedOffset, cached] of cache.pages) {
            if (
              cachedOffset === offset ||
              cachedOffset >= through ||
              (cached.nextOffset ?? cachedOffset + 25) <= offset
            )
              continue;
            cache.pages.set(cachedOffset, {
              ...cached,
              items: cached.items.filter(
                (child) =>
                  child.subtaskNumber <= offset ||
                  child.subtaskNumber > through,
              ),
            });
          }
        }
        cache.pages.delete(offset);
        if (
          page.hasMore === false ||
          (page.hasMore == null && offset + 25 >= page.totalCount)
        ) {
          for (const [cachedOffset, cached] of cache.pages)
            if (
              page.order === "priority"
                ? cached.items.length === 0 &&
                  (cached.cursor ?? "") >= (page.cursor ?? "")
                : cachedOffset > offset
            )
              cache.pages.delete(cachedOffset);
        }
        const returnedIdentities = new Set(page.items.map(identity));
        cache.pages.set(offset, {
          ...page,
          items: [
            ...page.items,
            ...retained.filter(
              (child) => !returnedIdentities.has(identity(child)),
            ),
          ],
          nextOffset: page.nextOffset ?? offset + 25,
          hasMore: page.hasMore ?? offset + 25 < page.totalCount,
        });
        cache.errors.delete(offset);
      } catch (error) {
        if (!controller.signal.aborted && isCurrent())
          cache.errors.set(
            offset,
            error instanceof Error ? error.message : String(error),
          );
      } finally {
        if (cache.inFlight.get(offset) === controller)
          cache.inFlight.delete(offset);
        if (!controller.signal.aborted && isCurrent())
          redraw((value) => value + 1);
      }
    },
    [beforeRowsChange, cache, enabled, parentId, view, visibleChildNumbers],
  );

  const refresh = useCallback(async () => {
    const epoch = activity.current.epoch;
    if (
      !activity.current.enabled ||
      activity.current.cache !== cache ||
      cache.inFlight.size > 0
    )
      return;
    const offsets = [...visibleOffsets.current].filter(
      (offset) => offset !== initialOffset && cache.pages.has(offset),
    );
    const cursors = new Map(
      offsets.map((offset) => [offset, cache.pages.get(offset)?.cursor]),
    );
    // Refresh the head first: its active cursor can move past cached windows.
    // Requests for the visible remainder must use the new coverage boundary.
    await fetchPage(initialOffset);
    // A hidden list or a view switch invalidates this whole refresh sequence,
    // including requests that have not started yet.
    if (
      !activity.current.enabled ||
      activity.current.cache !== cache ||
      activity.current.epoch !== epoch
    )
      return;
    const head = cache.pages.get(initialOffset);
    if (cache.indexing || cache.errors.has(initialOffset) || !head) return;
    if (
      (head.order === "priority" || view === "active") &&
      !head.legacyHistorical &&
      !head.hasMore
    )
      return;
    if (head.order === "priority") {
      // Preserve the loaded/visible windows, even when the first page now
      // crosses their former boundary. Unseen windows refresh on visibility.
      await Promise.all(
        offsets.map((offset) =>
          fetchPage(
            offset,
            (cursors.get(offset) ?? "") > (head.nextCursor ?? "")
              ? cursors.get(offset)
              : head.nextCursor,
          ),
        ),
      );
      return;
    }
    const remainder = new Set(
      offsets.map((offset) =>
        view === "active" && !head.legacyHistorical
          ? Math.max(offset, head.nextOffset ?? offset)
          : offset,
      ),
    );
    await Promise.all([...remainder].map((offset) => fetchPage(offset)));
  }, [cache, fetchPage, initialOffset, visibleOffsets, view]);
  const retry = useCallback(async () => {
    if (cache.errors.size === 0) return refresh();
    await Promise.all(
      [...cache.errors.keys()].map((offset) => fetchPage(offset)),
    );
  }, [cache, fetchPage, refresh]);
  useEffect(() => {
    activity.current = { cache, enabled, epoch: activity.current.epoch + 1 };
    if (!enabled) return;
    void refresh();
    const timer =
      refreshMs > 0
        ? window.setInterval(() => {
            if (!document.hidden && cache.inFlight.size === 0) void refresh();
          }, refreshMs)
        : undefined;
    return () => {
      if (activity.current.cache === cache)
        activity.current = {
          cache,
          enabled: false,
          epoch: activity.current.epoch + 1,
        };
      clearInterval(timer);
      for (const controller of cache.inFlight.values()) controller.abort();
      cache.inFlight.clear();
    };
  }, [cache, enabled, refresh, refreshMs]);

  // An older database may need a resumable index build before its first
  // sorted page is ready. Continue only while this list is actually visible.
  useEffect(() => {
    if (
      !enabled ||
      !cache.indexing ||
      cache.inFlight.size > 0 ||
      cache.errors.size > 0
    )
      return;
    let timer: number | undefined;
    const schedule = () => {
      window.clearTimeout(timer);
      if (!document.hidden)
        timer = window.setTimeout(() => {
          if (!document.hidden) void refresh();
        }, 200);
    };
    document.addEventListener("visibilitychange", schedule);
    schedule();
    return () => {
      window.clearTimeout(timer);
      document.removeEventListener("visibilitychange", schedule);
    };
  }, [
    cache,
    cache.indexing,
    cache.inFlight.size,
    cache.errors.size,
    enabled,
    refresh,
  ]);

  const pages = [...cache.pages.values()].sort((a, b) => a.offset - b.offset);
  const tail = pages
    .filter((page) => !page.detached)
    .reduce<ChildPage | undefined>(
      (furthest, page) =>
        !furthest ||
        (page.order === "priority"
          ? (page.nextCursor ?? "") > (furthest.nextCursor ?? "")
          : (page.nextOffset ?? page.offset) >
            (furthest.nextOffset ?? furthest.offset))
          ? page
          : furthest,
      undefined,
    );
  const rows = new Map<string, Child>();
  const rowOffsets = new Map<string, number>();
  // Map insertion order tracks which response is newest, independent of cursor order.
  for (const page of cache.pages.values()) {
    for (const child of page.items) {
      rows.set(identity(child), child);
      rowOffsets.set(identity(child), page.offset);
    }
  }
  const nextOffset = tail?.nextOffset ?? initialOffset;
  const loadMore = useCallback(() => {
    if (tail?.hasMore && nextOffset > tail.offset && cache.inFlight.size === 0)
      void fetchPage(nextOffset, tail.nextCursor);
  }, [cache, fetchPage, nextOffset, tail]);
  const latest = [...cache.pages.values()].at(-1);
  return {
    data: latest
      ? {
          ...latest,
          items: [...rows.values()].sort(
            view === "active" ? compareActiveSubtasks : compareAllSubtasks,
          ),
          partial: pages.some((page) => page.partial),
          hasMore: !!tail?.hasMore,
          nextOffset,
        }
      : undefined,
    pageOffset: (child: Child) =>
      rowOffsets.get(identity(child)) ?? initialOffset,
    error: cache.errors.values().next().value ?? null,
    loading:
      cache.inFlight.size > 0 || (cache.indexing && cache.errors.size === 0),
    loadingMore: [...cache.inFlight.keys()].some(
      (offset) => !cache.pages.has(offset),
    ),
    refresh,
    retry,
    loadMore,
  };
}
