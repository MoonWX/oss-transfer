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
with open(os.environ['CALLS']+'.args','a') as log: log.write(json.dumps(sys.argv[1:])+'\\n')
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
        for stale in (calls, Path(str(calls)+'.args')):
            if stale.exists(): stale.unlink()
        environment = dict(os.environ, PATH=str(self.root)+os.pathsep+os.environ['PATH'],
                           RESPONSES=json.dumps(responses), CALLS=str(calls))
        result = subprocess.run(['bash','-c','source "$1"; transfer_select_source "$2" "$3"',
                                 'test', str(ROOT/'deploy/native/select-source.sh'),source,version],
                                env=environment,text=True,capture_output=True)
        self.assertEqual(result.returncode == 0, success, result.stderr)
        return result.stdout.strip(), calls.read_text().splitlines() if calls.exists() else []

    def responses(self, country):
        return {
            'https://www.cloudflare.com/cdn-cgi/trace': [0,'ip=192.0.2.1\nloc='+country+'\n'],
            'https://api.github.com/repos/MoonWX/oss-transfer/releases/latest': [0,'{}'],
            'https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/manifest.tsv': [0,'manifest'],
        }

    INTERNAL = 'https://moonwx.s3-internal.cn-north-1.jdcloud-oss.com/oss-transfer'

    def arguments(self):
        return [json.loads(line) for line in (self.root / 'calls.args').read_text().splitlines()]

    def test_cn_uses_oss(self):
        source,calls=self.select(self.responses('CN'))
        self.assertEqual(source,'jdoss')
        # The unreachable internal endpoint is tried first, then location, then the mirror.
        self.assertEqual(calls,[self.INTERNAL+'/latest/manifest.tsv',
                                'https://www.cloudflare.com/cdn-cgi/trace',
                                'https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/manifest.tsv'])

    def test_reachable_internal_endpoint_is_preferred_without_location_lookup(self):
        for country in ('CN','US'):
            with self.subTest(country=country):
                data=self.responses(country)
                data[self.INTERNAL+'/latest/manifest.tsv']=[0,'manifest']
                source,calls=self.select(data)
                self.assertEqual(source,'jdoss-internal')
                self.assertEqual(calls,[self.INTERNAL+'/latest/manifest.tsv'])

    def test_internal_endpoint_must_hold_the_requested_version(self):
        data=self.responses('CN')
        data[self.INTERNAL+'/latest/manifest.tsv']=[0,'manifest']
        data['https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/v4.1.6/manifest.tsv']=[0,'manifest']
        source,calls=self.select(data,version='4.1.6')
        self.assertEqual(source,'jdoss')
        self.assertEqual(calls[0],self.INTERNAL+'/v4.1.6/manifest.tsv')
        data[self.INTERNAL+'/v4.1.6/manifest.tsv']=[0,'manifest']
        source,calls=self.select(data,version='v4.1.6')
        self.assertEqual(source,'jdoss-internal')
        self.assertEqual(calls,[self.INTERNAL+'/v4.1.6/manifest.tsv'])

    def test_internal_probe_gives_up_sooner_than_the_public_probes(self):
        self.select(self.responses('CN'))
        internal,_,public=self.arguments()
        self.assertEqual(internal[internal.index('--connect-timeout')+1],'1')
        self.assertEqual(internal[internal.index('--max-time')+1],'3')
        self.assertEqual(public[public.index('--connect-timeout')+1],'2')
        self.assertEqual(public[public.index('--max-time')+1],'5')
        for call in (internal,public):
            self.assertEqual(call[call.index('--proto')+1],'=https')

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
        self.assertIn('https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/v4.1.6/manifest.tsv',calls)
        self.assertFalse(any('/latest' in url for url in calls))

    def test_both_sources_unreachable_fails(self):
        self.select({},success=False)

    def test_explicit_sources_make_no_lookup_or_probe(self):
        for value in ('github','jdoss','jdoss-internal'):
            with self.subTest(source=value):
                selected,calls=self.select({},source=value)
                self.assertEqual(selected,value)
                self.assertEqual(calls,[])

    def test_old_official_release_urls_follow_the_renamed_prefix(self):
        legacy = 'https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer'
        official = 'https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer'
        for url, expected in (
            (legacy + '/latest/', official + '/latest'),
            (legacy + '/v4.1.7', official + '/v4.1.7'),
            (official + '/v4.1.7', official + '/v4.1.7'),
            (legacy + '-custom/v4.1.7', legacy + '-custom/v4.1.7'),
            ('https://mirror.example/osstransfer/v4.1.7', 'https://mirror.example/osstransfer/v4.1.7'),
        ):
            with self.subTest(url=url):
                result = subprocess.run(['bash', '-c', 'source "$1"; transfer_release_url "$2"',
                                         'test', str(ROOT/'deploy/native/select-source.sh'), url],
                                        check=True, text=True, capture_output=True)
                self.assertEqual(result.stdout.strip(), expected)


if __name__ == '__main__':
    unittest.main()
