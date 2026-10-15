#!/usr/bin/env python3
"""Exercise regional selection with recorded HTTP results, without geolocation calls."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class SourceSelection(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        shim = self.root / 'curl'
        shim.write_text('#!' + sys.executable + '\n' + '''
import json, os, sys
from pathlib import Path
url=next(arg for arg in sys.argv[1:] if arg.startswith('https://'))
with open(os.environ['CALLS'],'a') as log: log.write(url+'\\n')
responses=json.loads(os.environ['RESPONSES'])
status,body=responses.get(url,[22,''])
if status: sys.exit(status)
if '-o' in sys.argv:
    target=sys.argv[sys.argv.index('-o')+1]
    Path(target).write_text(body)
else: print(body,end='')
''')
        shim.chmod(0o755)

    def select(self, responses, source='auto', version='latest', success=True):
        calls = self.root / 'calls'
        environment = dict(os.environ, PATH=str(self.root)+os.pathsep+os.environ['PATH'],
                           RESPONSES=json.dumps(responses), CALLS=str(calls))
        result = subprocess.run(['bash','-c','source "$1"; transfer_select_source "$2" "$3"',
                                 'test', str(ROOT/'deploy/go/select-source.sh'),source,version],
                                env=environment,text=True,capture_output=True)
        self.assertEqual(result.returncode == 0, success, result.stderr)
        return result.stdout.strip(), calls.read_text().splitlines() if calls.exists() else []

    def responses(self, country):
        return {
            'https://www.cloudflare.com/cdn-cgi/trace': [0,'ip=192.0.2.1\nloc='+country+'\n'],
            'https://api.github.com/repos/MoonWX/oss-transfer/releases/latest': [0,'{}'],
            'https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest/manifest.tsv': [0,'manifest'],
        }

    def test_cn_uses_oss(self):
        source,calls=self.select(self.responses('CN'))
        self.assertEqual(source,'jdoss')
        self.assertEqual(len(calls),2)
        self.assertFalse(any('github' in url for url in calls))

    def test_non_cn_uses_github(self):
        for country in ('US','SG','HK','JP'):
            with self.subTest(country=country):
                source,_=self.select(self.responses(country))
                self.assertEqual(source,'github')

    def test_invalid_trace_uses_second_location_provider(self):
        data=self.responses('CN\nloc=US')
        data['https://ipinfo.io/country']=[0,'CN\r\n']
        source,calls=self.select(data)
        self.assertEqual(source,'jdoss')
        self.assertIn('https://ipinfo.io/country',calls)

    def test_lookup_timeout_defaults_to_github(self):
        data=self.responses('')
        data['https://www.cloudflare.com/cdn-cgi/trace']=[28,'']
        data['https://ipinfo.io/country']=[28,'']
        self.assertEqual(self.select(data)[0],'github')

    def test_failed_preferred_source_uses_same_version_on_other_source(self):
        data=self.responses('CN')
        data['https://api.github.com/repos/MoonWX/oss-transfer/releases/tags/v4.1.6']=[0,'{}']
        source,calls=self.select(data,version='4.1.6')
        self.assertEqual(source,'github')
        self.assertIn('https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/v4.1.6/manifest.tsv',calls)
        self.assertFalse(any('/latest' in url for url in calls))

    def test_both_sources_unreachable_fails(self):
        self.select({},success=False)

    def test_explicit_sources_make_no_lookup_or_probe(self):
        for value in ('github','jdoss'):
            with self.subTest(source=value):
                selected,calls=self.select({},source=value)
                self.assertEqual(selected,value)
                self.assertEqual(calls,[])


if __name__ == '__main__':
    unittest.main()
