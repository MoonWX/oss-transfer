import { formatDate, t } from "./i18n";

// An RFC 3339 time with its zone offset, as the Master returns time filters.
const withOffset =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(Z|[+-]\d{2}:\d{2})$/;

/**
 * A modification-time filter shown the way it was entered. The Master reads
 * the entered time in its own zone (the create form says which) and returns
 * it with that zone's offset, so the time is shown at that offset and
 * labelled with it, for example "2026/11/1 00:00:00（UTC）", rather than
 * converted to the browser's zone without a label. Returns undefined for a
 * value without an offset, such as one converted from v4.0.
 */
export function formatFilterTime(value: unknown): string | undefined {
  if (typeof value !== "string") return undefined;
  const match = withOffset.exec(value);
  const instant = Date.parse(value);
  if (!match || Number.isNaN(instant)) return undefined;
  const offset = match[1];
  const minutes =
    offset === "Z"
      ? 0
      : (offset[0] === "-" ? -1 : 1) *
        (Number(offset.slice(1, 3)) * 60 + Number(offset.slice(4, 6)));
  return t("{time}（{zone}）", {
    // The wall-clock time at that offset, formatted as UTC.
    time: formatDate(new Date(instant + minutes * 60_000), {
      timeZone: "UTC",
    }),
    zone: minutes === 0 ? "UTC" : `UTC${offset}`,
  });
}
