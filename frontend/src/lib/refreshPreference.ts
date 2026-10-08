import { useSyncExternalStore } from "react";

const STORAGE_KEY = "oss-transfer-refresh-seconds";
const CHANGE_EVENT = "oss-transfer-refresh-change";
export const DEFAULT_REFRESH_SECONDS = 10;
export const MIN_REFRESH_SECONDS = 1;
export const MAX_REFRESH_SECONDS = 3600;
export const REFRESH_PRESETS = [0, 1, 5, 10, 30, 60] as const;

export function isRefreshSeconds(value: number) {
  return (
    Number.isInteger(value) &&
    (value === 0 ||
      (value >= MIN_REFRESH_SECONDS && value <= MAX_REFRESH_SECONDS))
  );
}

function parseSavedValue(value: string | null) {
  const seconds = value === null || value.trim() === "" ? NaN : Number(value);
  return isRefreshSeconds(seconds) ? seconds : DEFAULT_REFRESH_SECONDS;
}

function readSavedValue() {
  try {
    return parseSavedValue(localStorage.getItem(STORAGE_KEY));
  } catch {
    return DEFAULT_REFRESH_SECONDS;
  }
}

let seconds = readSavedValue();
const getSnapshot = () => seconds;

function subscribe(onChange: () => void) {
  const onStorage = (event: StorageEvent) => {
    if (event.key !== STORAGE_KEY && event.key !== null) return;
    seconds = readSavedValue();
    onChange();
  };
  window.addEventListener(CHANGE_EVENT, onChange);
  window.addEventListener("storage", onStorage);
  return () => {
    window.removeEventListener(CHANGE_EVENT, onChange);
    window.removeEventListener("storage", onStorage);
  };
}

function setRefreshSeconds(value: number) {
  if (!isRefreshSeconds(value) || value === seconds) return;
  seconds = value;
  try {
    localStorage.setItem(STORAGE_KEY, String(value));
  } catch {
    // Keep the preference for this session if browser storage is unavailable.
  }
  window.dispatchEvent(new Event(CHANGE_EVENT));
}

export function useRefreshPreference() {
  const refreshSeconds = useSyncExternalStore(subscribe, getSnapshot);
  return {
    refreshSeconds,
    refreshMs: refreshSeconds * 1000,
    setRefreshSeconds,
  };
}

export const refreshDescription = (value: number) =>
  value === 0 ? "自动刷新已暂停" : `每 ${value} 秒刷新`;
