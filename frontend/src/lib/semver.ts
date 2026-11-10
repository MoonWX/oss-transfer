// Release versions follow the service's SemVer rules (internal/softwareupdate):
// MAJOR.MINOR.PATCH with an optional prerelease such as 4.1.29-beta.1, no
// leading zeros, no build metadata, and bounded lengths. *-dev builds are not
// releases and never compare.
type Parsed = { core: number[]; pre: string[] };

const numeric = /^(0|[1-9][0-9]{0,8})$/;
const identifier = /^[0-9A-Za-z-]{1,32}$/;

export function parseVersion(value: string): Parsed | null {
  if (!value || value.length > 64) return null;
  const dash = value.indexOf("-");
  const core = (dash < 0 ? value : value.slice(0, dash)).split(".");
  if (core.length !== 3 || !core.every((part) => numeric.test(part)))
    return null;
  if (dash < 0) return { core: core.map(Number), pre: [] };
  const pre = value.slice(dash + 1).split(".");
  if (pre.length > 8 || pre[0] === "dev") return null;
  for (const part of pre)
    if (
      !identifier.test(part) ||
      (/^[0-9]+$/.test(part) && !numeric.test(part))
    )
      return null;
  return { core: core.map(Number), pre };
}

export const validVersion = (value: string) => parseVersion(value) !== null;

export const isPrerelease = (value: string) =>
  (parseVersion(value)?.pre.length ?? 0) > 0;

function compareIdentifiers(a: string, b: string) {
  const an = /^[0-9]+$/.test(a),
    bn = /^[0-9]+$/.test(b);
  if (an && bn) return Math.sign(Number(a) - Number(b));
  if (an) return -1;
  if (bn) return 1;
  return a < b ? -1 : a > b ? 1 : 0;
}

/** Returns -1, 0 or 1 by SemVer precedence, or null for invalid versions. */
export function compareVersions(a: string, b: string): number | null {
  const x = parseVersion(a),
    y = parseVersion(b);
  if (!x || !y) return null;
  for (let i = 0; i < 3; i++)
    if (x.core[i] !== y.core[i]) return x.core[i] > y.core[i] ? 1 : -1;
  if (!x.pre.length || !y.pre.length)
    return x.pre.length === y.pre.length ? 0 : x.pre.length ? -1 : 1;
  for (let i = 0; i < Math.min(x.pre.length, y.pre.length); i++) {
    const result = compareIdentifiers(x.pre[i], y.pre[i]);
    if (result) return result;
  }
  return Math.sign(x.pre.length - y.pre.length);
}

export const newerVersion = (target: string, current: string) =>
  compareVersions(target, current) === 1;

/** A release version as users see it: "v4.1.42". Empty stays empty. */
export const displayVersion = (value?: string | null) =>
  !value ? "" : value.startsWith("v") ? value : `v${value}`;
