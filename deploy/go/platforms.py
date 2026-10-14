#!/usr/bin/env python3
"""Read the native release targets shared by builds and publication."""
import argparse
import json
from pathlib import Path


TARGETS = json.loads(Path(__file__).with_name("platforms.json").read_text(encoding="utf-8"))
PLATFORMS = [(row["os"], row["arch"]) for row in TARGETS]


def archive_name(os_name, arch):
    return f"oss-transfer-go-{os_name}-{arch}" + (".zip" if os_name == "windows" else ".tar.gz")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    operation = parser.add_mutually_exclusive_group(required=True)
    operation.add_argument("--list", action="store_true", help="space-separated build targets")
    operation.add_argument("--archives", action="store_true", help="one release archive per line")
    operation.add_argument("--go-env", metavar="PLATFORM", help="target OS, Go architecture and ARM level")
    args = parser.parse_args()
    if args.list:
        print(" ".join(os_name + "/" + arch for os_name, arch in PLATFORMS))
    elif args.archives:
        print("\n".join(archive_name(*platform) for platform in PLATFORMS))
    else:
        target = next((row for row in TARGETS if row["os"] + "/" + row["arch"] == args.go_env), None)
        if target is None:
            parser.error("unsupported native platform: " + args.go_env)
        print("\t".join((target["os"], target["goarch"], target.get("goarm", ""))))
