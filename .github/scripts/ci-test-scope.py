#!/usr/bin/env python3
"""Keep main release checks small; select relevant checks for pull requests."""
import json
import os
from pathlib import PurePosixPath
import re
import subprocess

COMPONENTS = ('console', 'server', 'cli', 'deployment')


def classify(paths):
    selected = set()
    for path in paths:
        name = PurePosixPath(path)
        if path.startswith('docs/') or name.suffix.lower() == '.md' or path == 'LICENSE':
            continue
        if path.startswith('frontend/'):
            selected.add('console')
        elif path.startswith(('src/main/', 'src/test/')):
            selected.update(('server', 'console'))
        elif path.startswith('cli-go/'):
            selected.add('cli')
        elif path.startswith('deploy/') or path in ('Dockerfile', '.dockerignore', 'docker-compose.yml', '.env.example'):
            selected.add('deployment')
        else:
            # Build inputs, CI changes and new/unclassified areas fail closed.
            return set(COMPONENTS)
    return selected


def choose(environ, event):
    # A fresh main commit must not hide a failure in an earlier commit by only
    # changing docs. Main and manual runs always keep the small release checks.
    if environ.get('GITHUB_EVENT_NAME') != 'pull_request':
        return set(COMPONENTS)
    try:
        base = event['pull_request']['base']['sha']
        head = event['pull_request']['head']['sha']
        if not all(re.fullmatch(r'[0-9a-f]{40}', value) for value in (base, head)):
            raise ValueError('Invalid comparison SHA')
        changed = subprocess.check_output(
            ['git', 'diff', '--name-only', '--no-renames', '-z', base + '...' + head],
            stderr=subprocess.DEVNULL, timeout=30)
        return classify(path for path in changed.decode('utf-8').split('\0') if path)
    except (KeyError, TypeError, ValueError, UnicodeError, subprocess.SubprocessError):
        print('Change comparison unavailable; running the core checks.')
        return set(COMPONENTS)


def main():
    with open(os.environ['GITHUB_EVENT_PATH'], encoding='utf-8') as source:
        event = json.load(source)
    selected = choose(os.environ, event)
    components = ','.join(part for part in ('server', 'cli') if part in selected) or 'none'
    values = {name: str(name in selected).lower() for name in COMPONENTS}
    values.update(build=str(bool(selected)).lower(), components=components)
    with open(os.environ['GITHUB_OUTPUT'], 'a', encoding='utf-8') as output:
        for name, value in values.items():
            output.write('{}={}\n'.format(name, value))
    print('Checks: ' + (', '.join(sorted(selected)) or 'documentation only'))


if __name__ == '__main__':
    main()
