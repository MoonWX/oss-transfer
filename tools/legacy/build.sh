#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
output=${1:-"$root/output/tools/legacy"}
mkdir -p "$output"
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
javac_bin=${JAVA_HOME:+$JAVA_HOME/bin/}javac
jar_bin=${JAVA_HOME:+$JAVA_HOME/bin/}jar
"$javac_bin" -source 8 -target 8 -encoding UTF-8 -d "$scratch" "$root/tools/legacy/LegacyExport.java"
"$jar_bin" cfe "$output/legacy-exporter.jar" LegacyExport -C "$scratch" .
echo "$output/legacy-exporter.jar"
