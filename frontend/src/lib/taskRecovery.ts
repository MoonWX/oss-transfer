import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import { api, ApiError, getToken } from "./api";

type Recovery = {
  phase: "submitting" | "accepted" | "checking" | "completed";
  generation?: number;
};
type States = Record<number, Recovery>;
const storageKey = "oss-transfer-task-recovery";
const listeners = new Set<() => void>();
let session = getToken();
let states: States = {};
try {
  const saved = JSON.parse(sessionStorage.getItem(storageKey) || "{}");
  if (session) {
    for (const [id, entry] of Object.entries(saved)) {
      const value = entry as Recovery;
      if (
        /^[1-9]\d*$/.test(id) &&
        ["accepted", "checking", "submitting", "completed"].includes(
          value?.phase,
        )
      )
        states[Number(id)] = {
          ...value,
          phase: value.phase === "submitting" ? "checking" : value.phase,
        };
    }
  }
} catch {
  // A missing or damaged browser cache must not prevent using the console.
}
let timer: ReturnType<typeof setTimeout> | undefined;
const checking = new Set<number>();
const nextCheck = new Map<number, number>();

function publish(next: States) {
  states = next;
  try {
    if (Object.keys(next).length)
      sessionStorage.setItem(storageKey, JSON.stringify(next));
    else sessionStorage.removeItem(storageKey);
  } catch {
    // The in-memory guard still works when browser storage is unavailable.
  }
  listeners.forEach((listener) => listener());
  schedule();
}

function currentSession() {
  const token = getToken();
  if (session !== token) {
    session = token;
    nextCheck.clear();
    publish({});
  }
  return token;
}

function release(id: number) {
  const next = { ...states };
  delete next[id];
  nextCheck.delete(id);
  publish(next);
}

function begin(ids: number[]) {
  currentSession();
  if (ids.some((id) => states[id])) return false;
  const next = { ...states };
  for (const id of ids) next[id] = { phase: "submitting" };
  publish(next);
  return true;
}

function accept(id: number, generation?: number) {
  nextCheck.set(id, Date.now() + 2000);
  publish({ ...states, [id]: { phase: "accepted", generation } });
}

function check(id: number) {
  nextCheck.set(id, Date.now());
  publish({ ...states, [id]: { ...states[id], phase: "checking" } });
}

function schedule() {
  if (
    timer ||
    !Object.values(states).some(
      (entry) => entry.phase === "accepted" || entry.phase === "checking",
    )
  )
    return;
  timer = setTimeout(() => {
    timer = undefined;
    if (!currentSession()) return;
    // Bound background work when a batch of tasks was resumed together.
    const due = Object.keys(states)
      .map(Number)
      .filter(
        (id) =>
          (states[id].phase === "accepted" ||
            states[id].phase === "checking") &&
          !checking.has(id) &&
          (nextCheck.get(id) || 0) <= Date.now(),
      )
      .sort((a, b) => (nextCheck.get(a) || 0) - (nextCheck.get(b) || 0))
      .slice(0, Math.max(0, 8 - checking.size));
    for (const id of due) void reconcile(id);
    schedule();
  }, 1000);
}

async function reconcile(id: number) {
  const entry = states[id];
  const token = session;
  checking.add(id);
  try {
    const { data } = await api<{
      data: { status: string; generation?: number };
    }>(`/api/master/tasks/${id}`, { signal: AbortSignal.timeout(10000) });
    if (token !== getToken() || states[id] !== entry) return;
    const finished = [
      "COMPLETED",
      "PARTIAL_FAILED",
      "FAILED",
      "STOPPED",
    ].includes(data.status);
    // An older snapshot cannot unlock a request already accepted for a newer
    // generation. An uncertain response is reconciled before allowing retry.
    if (
      finished &&
      (entry.generation === undefined ||
        (data.generation !== undefined && data.generation >= entry.generation))
    ) {
      if (data.status === "COMPLETED")
        publish({
          ...states,
          [id]: { phase: "completed", generation: data.generation },
        });
      else release(id);
    }
  } catch (cause) {
    if (
      states[id] === entry &&
      cause instanceof ApiError &&
      cause.status === 404
    )
      release(id);
  } finally {
    checking.delete(id);
    if (states[id]) nextCheck.set(id, Date.now() + 2000);
    schedule();
  }
}

function subscribe(listener: () => void) {
  currentSession();
  listeners.add(listener);
  schedule();
  return () => {
    listeners.delete(listener);
  };
}

export function useTaskRecovery() {
  const snapshot = useSyncExternalStore(subscribe, () => states);
  const token = getToken();
  // Each render's handlers own the entries they created. A late response from
  // an earlier action or login cannot change another action's guard.
  const owned = new Map<number, Recovery>();
  const isCurrent = () => token === getToken();
  const update = (id: number, operation: () => void) => {
    if (!isCurrent() || !owned.has(id) || owned.get(id) !== states[id]) return;
    operation();
    if (states[id]) owned.set(id, states[id]);
    else owned.delete(id);
  };
  const updateMany = (
    changes: {
      id: number;
      phase: "accepted" | "checking" | "release";
      generation?: number;
    }[],
  ) => {
    if (!isCurrent()) return;
    const next = { ...states };
    let changed = false;
    for (const change of changes) {
      if (!owned.has(change.id) || owned.get(change.id) !== states[change.id])
        continue;
      changed = true;
      if (change.phase === "release") {
        delete next[change.id];
        nextCheck.delete(change.id);
        owned.delete(change.id);
      } else {
        next[change.id] = {
          phase: change.phase,
          generation: change.generation,
        };
        nextCheck.set(
          change.id,
          Date.now() + (change.phase === "accepted" ? 2000 : 0),
        );
        owned.set(change.id, next[change.id]);
      }
    }
    if (changed) publish(next);
  };
  return {
    states: snapshot,
    settle: updateMany,
    isCurrent,
    begin: (ids: number[]) => {
      if (!isCurrent() || !begin(ids)) return false;
      for (const id of ids) owned.set(id, states[id]);
      return true;
    },
    accept: (id: number, generation?: number) =>
      update(id, () => accept(id, generation)),
    release: (id: number) => update(id, () => release(id)),
    check: (id: number) => update(id, () => check(id)),
  };
}

export type RecoveryTaskSnapshot = { status: string; generation?: number };
export function canRetrySnapshot(snapshot?: RecoveryTaskSnapshot) {
  return (
    !!snapshot &&
    ["PARTIAL_FAILED", "FAILED", "STOPPED"].includes(snapshot.status) &&
    Number.isSafeInteger(snapshot.generation) &&
    Number(snapshot.generation) > 0
  );
}

// Inspect only tasks represented on the current page. Failed snapshots stay at
// the generation the user saw; a later click must not silently retry a new one.
export function useVisibleRecoveryTasks(
  ids: number[],
  query: string,
  revision: unknown,
) {
  type View = {
    scope: string;
    tasks: Record<number, RecoveryTaskSnapshot | null>;
  };
  const token = getToken();
  const idsKey = JSON.stringify([...new Set(ids)].sort((a, b) => a - b));
  const scope = JSON.stringify([token, query, idsKey]);
  const current = useRef<View>({ scope: "", tasks: {} });
  const [view, setView] = useState(current.current);
  const update = (id: number, snapshot: RecoveryTaskSnapshot | null) => {
    if (token !== getToken() || current.current.scope !== scope) return;
    current.current = {
      scope,
      tasks: { ...current.current.tasks, [id]: snapshot },
    };
    setView(current.current);
  };
  useEffect(() => {
    const controller = new AbortController();
    let active = true,
      fetching = false;
    const selected = JSON.parse(idsKey) as number[];
    current.current = {
      scope,
      tasks: current.current.scope === scope ? current.current.tasks : {},
    };
    setView(current.current);
    const refresh = async (selectedIds: number[]) => {
      if (fetching || !active || token !== getToken()) return;
      fetching = true;
      let next = 0;
      await Promise.all(
        Array.from({ length: Math.min(4, selectedIds.length) }, async () => {
          while (active && token === getToken() && next < selectedIds.length) {
            const id = selectedIds[next++];
            try {
              const { data } = await api<{ data: RecoveryTaskSnapshot }>(
                `/api/master/tasks/${id}`,
                {
                  signal: AbortSignal.any([
                    controller.signal,
                    AbortSignal.timeout(10000),
                  ]),
                },
              );
              if (
                !data ||
                typeof data.status !== "string" ||
                !Number.isSafeInteger(data.generation) ||
                Number(data.generation) <= 0
              )
                throw new Error("Invalid task status");
              if (active && token === getToken()) update(id, data);
            } catch {
              if (active && token === getToken()) update(id, null);
            }
          }
        }),
      );
      fetching = false;
    };
    void refresh(selected);
    const timer = setInterval(() => {
      if (document.hidden) return;
      const pending = selected.filter((id) => {
        const value = current.current.tasks[id];
        return (
          !value ||
          [
            "RUNNING",
            "RESUMING",
            "LIST_DONE",
            "STOPPING",
            "PENDING",
            "CREATED",
            "AWAITING_RECOVERY",
          ].includes(value.status)
        );
      });
      if (pending.length) void refresh(pending);
    }, 3000);
    return () => {
      active = false;
      controller.abort();
      clearInterval(timer);
    };
  }, [scope, revision]);
  return {
    tasks: view.scope === scope && token === getToken() ? view.tasks : {},
    update,
  };
}
