import { useSyncExternalStore } from "react";
import { comparisonMessages } from "../locales/comparison";
import { coreMessages } from "../locales/core";
import { adminMessages } from "../locales/admin";
import { tasksMessages } from "../locales/tasks";
import { updatesMessages } from "../locales/updates";

export type Locale = "zh-CN" | "en-US" | "ja-JP";
export type Message = { en: string; ja: string };
export type MessageValues = Record<string, string | number>;
export const LOCALE_KEY = "oss-transfer-language";
export const languages: { locale: Locale; label: string }[] = [
  { locale: "zh-CN", label: "简体中文" },
  { locale: "en-US", label: "English" },
  { locale: "ja-JP", label: "日本語" },
];
export const messages: Record<string, Message> = {
  ...updatesMessages,
  ...tasksMessages,
  ...comparisonMessages,
  ...adminMessages,
  ...coreMessages,
};

export function normalizeLocale(
  value: string | null | undefined,
): Locale | undefined {
  const language = value
    ?.trim()
    .replaceAll("_", "-")
    .toLowerCase()
    .split("-")[0];
  return language === "zh"
    ? "zh-CN"
    : language === "ja"
      ? "ja-JP"
      : language === "en"
        ? "en-US"
        : undefined;
}

function browserLocale(): Locale {
  if (typeof navigator === "undefined") return "en-US";
  for (const value of navigator.languages ?? [navigator.language]) {
    const locale = normalizeLocale(value);
    if (locale) return locale;
  }
  return "en-US";
}

function initialLocale(): Locale {
  try {
    const saved = normalizeLocale(localStorage.getItem(LOCALE_KEY));
    if (saved) return saved;
  } catch {
    /* Language switching also works without persistent browser storage. */
  }
  return browserLocale();
}

let currentLocale = initialLocale();
const listeners = new Set<() => void>();
export const getLocale = (): Locale => currentLocale;
function applyLocale(locale: Locale) {
  const changed = currentLocale !== locale;
  currentLocale = locale;
  if (typeof document !== "undefined") document.documentElement.lang = locale;
  if (changed) listeners.forEach((listener) => listener());
}
export function setLocale(locale: Locale) {
  const supported = normalizeLocale(locale);
  if (!supported) return;
  try {
    localStorage.setItem(LOCALE_KEY, supported);
  } catch {
    /* Use the in-memory preference. */
  }
  applyLocale(supported);
}
if (typeof window !== "undefined") {
  applyLocale(currentLocale);
  window.addEventListener("storage", (event) => {
    if (event.key === LOCALE_KEY || event.key === null)
      applyLocale(normalizeLocale(event.newValue) ?? browserLocale());
  });
}
const subscribe = (listener: () => void) => {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
};

/** Only application messages are translated; interpolation values stay verbatim. */
export function translate(
  locale: Locale,
  source: string,
  values: MessageValues = {},
): string {
  const entry = Object.hasOwn(messages, source) ? messages[source] : undefined;
  const template =
    locale === "zh-CN"
      ? source
      : (entry?.[locale === "ja-JP" ? "ja" : "en"] ?? source);
  return template.replace(
    /\{([A-Za-z][A-Za-z0-9_]*)\}/g,
    (placeholder, key: string) =>
      Object.hasOwn(values, key) ? String(values[key]) : placeholder,
  );
}
export const t = (source: string, values?: MessageValues) =>
  translate(currentLocale, source, values);
export function useI18n() {
  const locale = useSyncExternalStore(
    subscribe,
    getLocale,
    () => "en-US" as Locale,
  );
  return { locale, t, setLocale };
}
export function formatNumber(
  value: unknown,
  options?: Intl.NumberFormatOptions,
) {
  return Number(value || 0).toLocaleString(currentLocale, options);
}
export function formatDate(
  value: unknown,
  options?: Intl.DateTimeFormatOptions,
) {
  if (value == null || value === "") return "—";
  const date =
    value instanceof Date ? value : new Date(value as string | number);
  return Number.isNaN(date.getTime())
    ? "—"
    : date.toLocaleString(currentLocale, { hour12: false, ...options });
}
