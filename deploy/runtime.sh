#!/usr/bin/env bash
set -euo pipefail
[[ $# == 3 ]] || { printf 'Usage: runtime.sh JAVA JAR CONFIG\n' >&2; exit 1; }
read -r -a java_options <<< "${JAVA_OPTS:--Xms128m -Xmx1024m}"
exec "$1" "${java_options[@]}" -jar "$2" "--spring.config.additional-location=file:$3"
