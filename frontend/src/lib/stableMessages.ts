import { useEffect, useRef, useState } from "react";

/** How long a runtime alert must persist before it appears, and stay absent before it goes. */
export const STABLE_MESSAGE_MS = 4000;

type Entry = {
  firstSeen: number;
  seen: number;
  absentSince: number;
  absent: number;
  shown: boolean;
};

/**
 * Hysteresis for runtime alerts that a single reconciliation step can raise
 * and clear again. A message appears once it has been present in two
 * consecutive snapshots (one when automatic refresh is off) and for at least
 * `stableMs`; once shown, it stays until it has been absent for as many
 * snapshots and as long. `snapshot` must change identity with every refresh.
 * Disabled, the messages pass through unchanged (for example once a task has
 * ended and its error no longer changes).
 */
export function useStableMessages(
  messages: readonly string[],
  snapshot: unknown,
  {
    enabled = true,
    refreshMs,
    scope,
    stableMs = STABLE_MESSAGE_MS,
  }: {
    enabled?: boolean;
    refreshMs: number;
    /** Starts over when this changes, for example for another task. */
    scope?: unknown;
    stableMs?: number;
  },
): string[] {
  const entries = useRef(new Map<string, Entry>());
  const last = useRef<{ snapshot: unknown; scope: unknown }>(undefined);
  const [, rerender] = useState(0);
  const now = Date.now();
  const minimum = refreshMs > 0 ? 2 : 1;
  if (!enabled || (last.current && last.current.scope !== scope)) {
    entries.current.clear();
    last.current = undefined;
  }
  if (enabled && (!last.current || last.current.snapshot !== snapshot)) {
    // Count each refresh once, however often this component renders.
    last.current = { snapshot, scope };
    const present = new Set(messages);
    for (const message of present) {
      const entry = entries.current.get(message);
      if (!entry)
        entries.current.set(message, {
          firstSeen: now,
          seen: 1,
          absentSince: 0,
          absent: 0,
          shown: false,
        });
      else {
        entry.seen++;
        entry.absent = 0;
      }
    }
    for (const [message, entry] of entries.current) {
      if (present.has(message)) continue;
      // A message that never became visible starts over when it returns.
      if (!entry.shown) entries.current.delete(message);
      else if (entry.absent++ === 0) entry.absentSince = now;
    }
  }
  let due = Infinity;
  if (enabled)
    for (const [message, entry] of entries.current) {
      if (!entry.shown) {
        const at = entry.firstSeen + stableMs;
        if (entry.seen >= minimum && now >= at) entry.shown = true;
        else if (now < at) due = Math.min(due, at);
      } else if (entry.absent > 0) {
        const at = entry.absentSince + stableMs;
        if (entry.absent >= minimum && now >= at)
          entries.current.delete(message);
        else if (now < at) due = Math.min(due, at);
      }
    }
  // Re-evaluate when a threshold passes, even if no refresh arrives then.
  useEffect(() => {
    if (!Number.isFinite(due)) return;
    const timer = window.setTimeout(
      () => rerender((value) => value + 1),
      Math.max(0, due - Date.now()),
    );
    return () => window.clearTimeout(timer);
  }, [due]);
  if (!enabled) return [...new Set(messages)];
  return [...entries.current]
    .filter(([, entry]) => entry.shown)
    .map(([message]) => message);
}
