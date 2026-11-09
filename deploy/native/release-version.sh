# shellcheck shell=bash
# Shared by the standalone Unix installers. Succeeds when release version $1
# has lower Semantic Versioning precedence than $2 (4.1.41 < 4.2.0-rc.1 <
# 4.2.0). Build metadata is ignored; an unparsable version never compares.
transfer_version_older() {
  LC_ALL=C awk -v left="$1" -v right="$2" '
  function parse(value, parts,   dash, core, fields, i) {
    sub(/\+.*/, "", value)
    dash = index(value, "-")
    if (dash) { parts["pre"] = substr(value, dash + 1); core = substr(value, 1, dash - 1) }
    else { parts["pre"] = ""; core = value }
    if (split(core, fields, ".") != 3) return 0
    for (i = 1; i <= 3; i++) { if (fields[i] !~ /^[0-9]+$/) return 0; parts[i] = fields[i] + 0 }
    return dash == 0 || parts["pre"] ~ /^[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*$/
  }
  function prerelease_before(x, y,   xs, ys, nx, ny, i, xn, yn) {
    nx = split(x, xs, "."); ny = split(y, ys, ".")
    for (i = 1; i <= nx && i <= ny; i++) {
      xn = xs[i] ~ /^[0-9]+$/; yn = ys[i] ~ /^[0-9]+$/
      if (xn && yn) { if (xs[i] + 0 != ys[i] + 0) return xs[i] + 0 < ys[i] + 0 }
      else if (xn != yn) return xn
      else if (xs[i] "" != ys[i] "") return xs[i] "" < ys[i] ""
    }
    return nx < ny
  }
  BEGIN {
    if (!parse(left, a) || !parse(right, b)) exit 1
    for (i = 1; i <= 3; i++) if (a[i] != b[i]) exit !(a[i] < b[i])
    if (a["pre"] == b["pre"] || a["pre"] == "") exit 1
    if (b["pre"] == "") exit 0
    exit !prerelease_before(a["pre"], b["pre"])
  }'
}
