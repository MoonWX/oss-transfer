import { translate, type Locale } from "./i18n";

export function buildAge(now: number, builtAt: number, locale: Locale) {
  const relativeTime = new Intl.RelativeTimeFormat(locale, {
    style: "short",
  });
  const elapsed = Math.max(0, Math.floor((now - builtAt) / 1000));
  const units = [
    [31536000, "year"],
    [2592000, "month"],
    [86400, "day"],
    [3600, "hour"],
    [60, "minute"],
  ] as const;
  for (const [seconds, unit] of units) {
    if (elapsed >= seconds) {
      const value = -Math.floor(elapsed / seconds);
      if (locale === "zh-CN") {
        // Intl emits "4天前"; separate the number from the Chinese unit.
        return relativeTime
          .formatToParts(value, unit)
          .map((part) =>
            part.type === "integer" ? `${part.value} ` : part.value,
          )
          .join("");
      }
      return relativeTime.format(value, unit);
    }
  }
  return translate(locale, "不到 1 分钟前");
}

/** "v4.1.42 (33b320a)": the release version first, the commit as detail. */
export function buildLabel(release: string | undefined, revision: string) {
  const name = release?.trim().replace(/^v/, "");
  return name ? `v${name} (${revision})` : revision;
}
