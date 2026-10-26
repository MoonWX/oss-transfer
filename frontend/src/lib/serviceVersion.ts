import { useSyncExternalStore } from "react";

declare const __BUILD_INFO__: { commit: string; builtAt: string };

const commitPattern = /^[0-9a-f]{7,40}(?:-dirty)?$/i;
const clientCommit = __BUILD_INFO__.commit;
const listeners = new Set<() => void>();
let updated = false;

export function observeServerCommit(commit: string | null) {
  if (
    updated ||
    !commit ||
    !commitPattern.test(clientCommit) ||
    !commitPattern.test(commit) ||
    commit.toLowerCase() === clientCommit.toLowerCase()
  )
    return;
  // Keep the notice until this document is reloaded. A delayed response from
  // the old process must not hide an update already observed.
  updated = true;
  listeners.forEach((listener) => listener());
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function useServiceUpdated() {
  return useSyncExternalStore(subscribe, () => updated);
}
