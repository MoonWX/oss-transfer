#!/usr/bin/env python3
"""Keep the shared download code embedded in both standalone Unix installers."""
import argparse
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
START = '# BEGIN embedded download source\n'
END = '# END embedded download source\n'


def sync(check=False):
    body = START + ''.join((ROOT / 'deploy/go' / name).read_text() + '\n'
                           for name in ('select-source.sh', 'download-source.sh')) + END
    for name in ('install.sh', 'deploy/go/install.sh'):
        path = ROOT / name
        text = path.read_text()
        if START in text:
            before, tail = text.split(START, 1)
            _, after = tail.split(END, 1)
        else:
            before, after = text.split('umask 077\n', 1)
            before += 'umask 077\n'
        updated = before + body + after
        if check:
            if text != updated:
                raise SystemExit(f'{name}: run python3 scripts/sync-installer-source.py')
        else:
            path.write_text(updated)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--check', action='store_true')
    sync(parser.parse_args().check)
