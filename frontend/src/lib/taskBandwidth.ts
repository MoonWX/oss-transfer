import { t as tr, getLocale } from "./i18n";
export const TASK_BANDWIDTH_MIN_MBPS = 0.1;
export const TASK_BANDWIDTH_MAX_MBPS = 1_000_000;
export const TASK_BANDWIDTH_ERROR = "带宽上限需为 0.1–1,000,000 Mbps，或留空。";

export function validateTaskBandwidth(value: unknown): number | null {
  if (value == null) return null;
  if (
    typeof value !== "number" ||
    !Number.isFinite(value) ||
    value < TASK_BANDWIDTH_MIN_MBPS ||
    value > TASK_BANDWIDTH_MAX_MBPS
  )
    throw new Error(tr(TASK_BANDWIDTH_ERROR));
  return value;
}

export const parseTaskBandwidth = (value: string) =>
  validateTaskBandwidth(value.trim() === "" ? null : Number(value));

export const formatTaskBandwidth = (value: number) =>
  `${value.toLocaleString(getLocale(), { maximumFractionDigits: 6 })} Mbps`;
