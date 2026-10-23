import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type RefObject,
} from "react";
import { api } from "./api";

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
};
type Cache = {
  pages: Map<number, ChildPage>;
  errors: Map<number, string>;
  inFlight: Map<number, AbortController>;
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
}: {
  parentId?: number;
  view: ChildView;
  initialOffset: number;
  enabled: boolean;
  refreshMs: number;
  visibleOffsets: RefObject<Set<number>>;
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
    });
  const cache = caches.current.get(key)!;
  const activity = useRef({ cache, enabled, epoch: 0 });
  if (activity.current.cache !== cache || activity.current.enabled !== enabled)
    activity.current = { cache, enabled, epoch: activity.current.epoch + 1 };
  const fetchPage = useCallback(
    async (offset: number) => {
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
        const result = await api<{ data: ChildPage }>(
          `/api/master/tasks/${parentId}/children?offset=${offset}&limit=25${view === "active" ? "&view=active" : ""}`,
          {
            signal: AbortSignal.any([
              controller.signal,
              AbortSignal.timeout(120000),
            ]),
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
        if (view === "active" && !page.legacyHistorical) {
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
          for (const cachedOffset of cache.pages.keys())
            if (cachedOffset > offset) cache.pages.delete(cachedOffset);
        }
        cache.pages.set(offset, {
          ...page,
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
    [cache, enabled, parentId, view],
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
    if (cache.errors.has(initialOffset) || !head) return;
    if (view === "active" && !head.legacyHistorical && !head.hasMore) return;
    const remainder = new Set(
      offsets.map((offset) =>
        view === "active" && !head.legacyHistorical
          ? Math.max(offset, head.nextOffset ?? offset)
          : offset,
      ),
    );
    await Promise.all([...remainder].map(fetchPage));
  }, [cache, fetchPage, initialOffset, visibleOffsets, view]);
  const retry = useCallback(async () => {
    if (cache.errors.size === 0) return refresh();
    await Promise.all([...cache.errors.keys()].map(fetchPage));
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

  const pages = [...cache.pages.values()].sort((a, b) => a.offset - b.offset);
  const tail = pages.reduce<ChildPage | undefined>(
    (furthest, page) =>
      !furthest ||
      (page.nextOffset ?? page.offset) >
        (furthest.nextOffset ?? furthest.offset)
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
      void fetchPage(nextOffset);
  }, [cache, fetchPage, nextOffset, tail]);
  const latest = [...cache.pages.values()].at(-1);
  return {
    data: latest
      ? {
          ...latest,
          items: [...rows.values()],
          partial: pages.some((page) => page.partial),
          hasMore: !!tail?.hasMore,
          nextOffset,
        }
      : undefined,
    pageOffset: (child: Child) =>
      rowOffsets.get(identity(child)) ?? initialOffset,
    error: cache.errors.values().next().value ?? null,
    loading: cache.inFlight.size > 0,
    loadingMore: [...cache.inFlight.keys()].some(
      (offset) => !cache.pages.has(offset),
    ),
    refresh,
    retry,
    loadMore,
  };
}
