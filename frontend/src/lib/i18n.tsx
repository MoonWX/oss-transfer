import { useSyncExternalStore } from "react";
import type { Catalog } from "../locales";

export type Locale = "zh-CN" | "en-US" | "ja-JP";
export type MessageValues = Record<string, string | number>;
export const LOCALE_KEY = "oss-transfer-language";
export const languages: { locale: Locale; label: string }[] = [
  { locale: "zh-CN", label: "简体中文" },
  { locale: "en-US", label: "English" },
  { locale: "ja-JP", label: "日本語" },
];

type CatalogLanguage = "en" | "ja";
// Chinese source strings are the keys, so zh-CN needs no table. The English
// and Japanese tables are separate chunks fetched only when selected.
const catalogs: Partial<Record<CatalogLanguage, Catalog>> = {};
const catalogLoaders: Record<CatalogLanguage, () => Promise<Catalog>> = {
  en: () => import("virtual:locale-catalog/en").then((m) => m.default),
  ja: () => import("virtual:locale-catalog/ja").then((m) => m.default),
};
const pendingCatalogs: Partial<Record<CatalogLanguage, Promise<boolean>>> = {};
const catalogLanguage = (locale: Locale): CatalogLanguage | undefined =>
  locale === "en-US" ? "en" : locale === "ja-JP" ? "ja" : undefined;

/** Installs a translation table, e.g. one built from the sources in tests. */
export function registerCatalog(language: CatalogLanguage, catalog: Catalog) {
  catalogs[language] = catalog;
}

/** Resolves to whether messages for the locale can be translated. */
export function loadLocale(locale: Locale): Promise<boolean> {
  const language = catalogLanguage(locale);
  if (!language || catalogs[language]) return Promise.resolve(true);
  return (pendingCatalogs[language] ??= catalogLoaders[language]().then(
    (catalog) => {
      registerCatalog(language, catalog);
      return true;
    },
    () => {
      // Allow a later attempt to fetch the table again.
      delete pendingCatalogs[language];
      return false;
    },
  ));
}

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

const startupLocale = initialLocale();
// In the browser the page shows a language only once its table is loaded, so
// it starts from the Chinese source text and switches when the table arrives.
let currentLocale: Locale =
  typeof window === "undefined" ? startupLocale : "zh-CN";
let requestedLocale = currentLocale;
// Start fetching the table while the rest of the application loads.
const startupCatalog = loadLocale(startupLocale);
const listeners = new Set<() => void>();
export const getLocale = (): Locale => currentLocale;
/** Resolves when the language chosen at startup can be rendered. */
export const localeReady = (): Promise<void> => startupCatalog.then(() => {});
function applyLocale(locale: Locale, onUnavailable?: () => void) {
  requestedLocale = locale;
  const show = () => {
    // A newer choice made while this table was loading takes precedence.
    if (requestedLocale !== locale) return;
    const changed = currentLocale !== locale;
    currentLocale = locale;
    if (typeof document !== "undefined") document.documentElement.lang = locale;
    if (changed) listeners.forEach((listener) => listener());
  };
  const language = catalogLanguage(locale);
  if (!language || catalogs[language]) show();
  else
    void loadLocale(locale).then((loaded) => {
      if (loaded) show();
      else if (requestedLocale === locale) onUnavailable?.();
    });
}
export function setLocale(locale: Locale) {
  const supported = normalizeLocale(locale);
  if (!supported) return;
  try {
    localStorage.setItem(LOCALE_KEY, supported);
  } catch {
    /* Use the in-memory preference. */
  }
  applyLocale(supported, () => {
    // After a master upgrade an open tab still refers to the previous
    // build's table, which no longer exists. Reloading picks up the current
    // build with the saved choice; offline, the page keeps its language.
    if (typeof navigator === "undefined" || navigator.onLine)
      window.location.reload();
  });
}
if (typeof window !== "undefined") {
  applyLocale(startupLocale);
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

// English catalogs write counts in the plural ("{count} tasks"). A count of
// exactly one takes the singular noun; other languages need no change.
const PLURAL_NOUNS =
  /\{([A-Za-z][A-Za-z0-9_]*)\} (seconds|minutes|hours|days|objects|files|tasks|nodes|agents|records|requests|bytes|retries)\b/g;
function singularize(template: string, values: MessageValues) {
  return template.replace(PLURAL_NOUNS, (match, key: string, noun: string) =>
    Object.hasOwn(values, key) && String(values[key]) === "1"
      ? `{${key}} ${noun === "retries" ? "retry" : noun.slice(0, -1)}`
      : match,
  );
}

/** Only application messages are translated; interpolation values stay verbatim. */
export function translate(
  locale: Locale,
  source: string,
  values: MessageValues = {},
): string {
  const language = catalogLanguage(locale);
  const catalog = language ? catalogs[language] : undefined;
  let template =
    catalog && Object.hasOwn(catalog, source) ? catalog[source] : source;
  if (language === "en") template = singularize(template, values);
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
