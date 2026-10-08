import { useEffect, useState } from "react";

const STORAGE_KEY = "oss-transfer-sidebar";
export const SIDEBAR_DEFAULT_WIDTH = 248;
export const SIDEBAR_MIN_WIDTH = 216;
export const SIDEBAR_MAX_WIDTH = 360;
export const SIDEBAR_COLLAPSED_WIDTH = 80;
export const clampSidebarWidth = (value: number) =>
  Math.max(SIDEBAR_MIN_WIDTH, Math.min(SIDEBAR_MAX_WIDTH, Math.round(value)));

function readPreference() {
  try {
    const value = JSON.parse(localStorage.getItem(STORAGE_KEY) || "null");
    return {
      collapsed: value?.collapsed === true,
      width: Number.isFinite(value?.width)
        ? clampSidebarWidth(value.width)
        : SIDEBAR_DEFAULT_WIDTH,
    };
  } catch {
    return { collapsed: false, width: SIDEBAR_DEFAULT_WIDTH };
  }
}

export function useSidebarPreference() {
  const [preference, setPreference] = useState(readPreference);
  useEffect(() => {
    try {
      localStorage.setItem(STORAGE_KEY, JSON.stringify(preference));
    } catch {
      // Keep controls usable when browser storage is unavailable.
    }
  }, [preference]);
  return {
    ...preference,
    toggle: () =>
      setPreference((current) => ({
        ...current,
        collapsed: !current.collapsed,
      })),
    resize: (width: number) =>
      setPreference((current) => ({
        ...current,
        width: clampSidebarWidth(width),
      })),
  };
}
