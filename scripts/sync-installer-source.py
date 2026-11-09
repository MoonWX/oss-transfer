#!/usr/bin/env python3
"""Keep the shared download and version code embedded in both standalone Unix installers.

install.sh also embeds the native installer, so a copied GitHub archive plus
install.sh can be installed offline when native-install.sh was not copied.
"""
import argparse
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
START = '# BEGIN embedded download source\n'
END = '# END embedded download source\n'
NATIVE_START = '# BEGIN embedded native installer\n'
NATIVE_END = '# END embedded native installer\n'
DELIMITER = 'OSS_TRANSFER_EMBEDDED_NATIVE_INSTALLER'


def replace_block(text, start, end, body, anchor):
    if start in text:
        before, tail = text.split(start, 1)
        _, after = tail.split(end, 1)
    else:
        before, after = text.split(anchor, 1)
        before += anchor
    return before + body + after


def sync(check=False):
    body = START + ''.join((ROOT / 'deploy/native' / name).read_text() + '\n'
                           for name in ('select-source.sh', 'download-source.sh', 'release-version.sh')) + END
    updated = {}
    for name in ('deploy/native/install.sh', 'install.sh'):
        updated[name] = replace_block((ROOT / name).read_text(), START, END, body, 'umask 077\n')
    native = updated['deploy/native/install.sh']
    if DELIMITER in native or not native.endswith('\n'):
        raise SystemExit('deploy/native/install.sh cannot be embedded in install.sh')
    native_body = (NATIVE_START + '# Generated from deploy/native/install.sh; used only when an offline package lacks native-install.sh.\n'
                   + 'transfer_embedded_native_installer() {\n'
                   + f"  cat <<'{DELIMITER}'\n" + native + DELIMITER + '\n}\n' + NATIVE_END)
    updated['install.sh'] = replace_block(updated['install.sh'], NATIVE_START, NATIVE_END, native_body, END)
    for name, text in updated.items():
        path = ROOT / name
        if check:
            if path.read_text() != text:
                raise SystemExit(f'{name}: run python3 scripts/sync-installer-source.py')
        else:
            path.write_text(text)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--check', action='store_true')
    sync(parser.parse_args().check)
