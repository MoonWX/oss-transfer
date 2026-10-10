#!/usr/bin/env python3
"""Guard against accidentally skipping checks for executable changes."""
import importlib.util
from pathlib import Path
import subprocess
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location('scope', Path(__file__).resolve().parents[1] / 'scripts/ci-test-scope.py')
scope = importlib.util.module_from_spec(spec)
spec.loader.exec_module(scope)


class ScopeTests(unittest.TestCase):
    def test_docs_pr_skips_but_main_keeps_release_checks(self):
        self.assertEqual(scope.classify(['README.md', 'docs/deploy.md', '.github/RELEASING.md']), set())
        for event in ('push', 'workflow_dispatch'):
            self.assertEqual(scope.choose({'GITHUB_EVENT_NAME': event}, {}), set(scope.COMPONENTS))

    def test_components_and_unknown_inputs(self):
        self.assertEqual(scope.classify(['frontend/src/App.tsx', 'cli-go/main.go']), {'console', 'cli'})
        self.assertEqual(scope.classify(['src/main/java/Transfer.java']), {'server', 'console'})
        self.assertEqual(scope.classify(['deploy/install.sh', 'Dockerfile']), {'deployment'})
        for path in ('pom.xml', 'build.sh', '.github/workflows/ci.yml', 'new-engine/main.go'):
            self.assertEqual(scope.classify([path]), set(scope.COMPONENTS))

    def test_pull_request_comparison_includes_deleted_and_renamed_sources(self):
        event = {'pull_request': {'base': {'sha': 'a' * 40}, 'head': {'sha': 'b' * 40}}}
        with mock.patch.object(scope.subprocess, 'check_output', return_value=b'frontend/src/old.ts\0docs/moved.md\0') as diff:
            self.assertEqual(scope.choose({'GITHUB_EVENT_NAME': 'pull_request'}, event), {'console'})
            self.assertIn('--no-renames', diff.call_args[0][0])
            self.assertEqual(diff.call_args[0][0][-1], 'a' * 40 + '...' + 'b' * 40)

    def test_missing_or_invalid_base_does_not_skip_checks(self):
        env = {'GITHUB_EVENT_NAME': 'pull_request'}
        self.assertEqual(scope.choose(env, {}), set(scope.COMPONENTS))
        event = {'pull_request': {'base': {'sha': 'a' * 40}, 'head': {'sha': 'b' * 40}}}
        with mock.patch.object(scope.subprocess, 'check_output', side_effect=subprocess.CalledProcessError(128, 'git')):
            self.assertEqual(scope.choose(env, event), set(scope.COMPONENTS))


if __name__ == '__main__':
    unittest.main()
