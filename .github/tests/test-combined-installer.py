#!/usr/bin/env python3
"""Run the public installer through Master/Agent selection and identity checks.

The native installer and CLI are process fixtures: this checks the real wrapper's
arguments, discovery and ordering without installing services on the test host.
"""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
INSTALLER = ROOT / "install.sh"

NATIVE_FIXTURE = r'''
import json, os, shutil, sys
from pathlib import Path

args = sys.argv[1:]
def option(name, default=None):
    return args[args.index(name)+1] if name in args else default
def event(value):
    with open(os.environ['INSTALLER_EVENTS'], 'a') as stream:
        stream.write(json.dumps(value) + '\n')
role = option('--role')
event({'event': 'native', 'role': role, 'args': args,
       'lang': os.environ.get('OSS_TRANSFER_INSTALL_LANG'),
       'token': os.environ.get('TRANSFER_JOIN_TOKEN', '')})
if os.environ.get('NATIVE_RESULT'):
    # Like a native installer of this release, report whether anything changed.
    with open(os.environ['OSS_TRANSFER_INSTALL_RESULT'], 'a') as stream:
        stream.write(os.environ['NATIVE_RESULT'] + '\toss-transfer-' + role + '\n')
if role != 'master':
    print('Agent installation finished')
    sys.exit(int(os.environ.get('AGENT_EXIT_CODE', '0')))
root = Path(option('--root-dir', os.environ.get('DEFAULT_INSTALL_ROOT')))
install = Path(option('--install-dir', os.environ.get('MASTER_INSTALL_DIR', str(root/'master/bin'))))
config = Path(option('--config-dir', str(root/'master/config')))/'service.json'
data = root/'master/data'
cli = Path(option('--cli-path', str(install/'oss-transfer-cli')))
for directory in (install, config.parent, data, data/'packages', cli.parent):
    directory.mkdir(parents=True, exist_ok=True)
config.write_text(json.dumps({'dataDirectory': str(data)}))
(data/'cli-cluster.json').write_text(json.dumps({'masterUrl': os.environ['MASTER_ORIGIN']}))
service = option('--service-name', 'oss_transfer_master' if os.environ['FIXTURE_OS'] == 'FreeBSD' else 'oss-transfer-master')
(install/'install.tsv').write_text('role\tmaster\nconfig\t'+str(config)+'\ndataDir\t'+str(data)+'\ncliPath\t'+str(cli)+'\nserviceName\t'+service+'\n')
shutil.copyfile(Path(option('--package-dir'))/'manifest.tsv', data/'packages/manifest.tsv')
(install/'oss-transfer-master').write_text('#!/bin/sh\nprintf "listen\\t0.0.0.0:1685\\n"\n')
(install/'oss-transfer-master').chmod(0o755)
shutil.copyfile(os.environ['FIXTURE_CLI'], cli)
cli.chmod(0o755)
print('Installation: '+str(install))
zh = os.environ.get('OSS_TRANSFER_INSTALL_LANG') == 'zh'
if os.environ.get('NATIVE_RESULT') != 'unchanged':
    # A skipped same-version run prints no access details.
    print(('访问地址: ' if zh else 'Web UI: ')+os.environ['MASTER_ORIGIN'])
if os.environ.get('NEW_PASSWORD'):
    print('初始管理员: admin' if zh else 'Initial administrator: admin')
    print(('初始密码: ' if zh else 'Initial password: ')+os.environ['NEW_PASSWORD'])
'''

CLI_FIXTURE = r'''
import json, os, sys
args = sys.argv[1:]
with open(os.environ['INSTALLER_EVENTS'], 'a') as stream:
    stream.write(json.dumps({'event': 'invite', 'args': args})+'\n')
if args[args.index('--host')+1] != os.environ['MASTER_ORIGIN']:
    sys.exit(2)
if args[args.index('--master-url')+1] != os.environ['MASTER_ORIGIN']:
    sys.exit(2)
print(json.dumps({'inviteToken': 'local-agent-invitation-fixture-123456'}))
'''

CURL_FIXTURE = r'''
import json, os, sys
args = sys.argv[1:]
config = sys.stdin.read() if '--config' in args else ''
with open(os.environ['INSTALLER_EVENTS'], 'a') as stream:
    stream.write(json.dumps({'event': 'verify', 'args': args, 'config': config})+'\n')
# Like curl 7.29 (CentOS 7), URL brackets are a glob range unless disabled.
if any('://' in arg and '[' in arg for arg in args) and not {'-g', '--globoff'} & set(args):
    sys.exit(3)
origin = os.environ['MASTER_ORIGIN']
expected = 'header = "Authorization: Bearer '+os.environ['ACCEPTED_AGENT_TOKEN']+'"\n'
if origin+'/api/auth/current' not in args or config != expected:
    sys.exit(22)
'''


class CombinedInstallerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='combined-installer-')
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.cluster = self.directory/'chosen cluster'
        self.packages = self.directory/'packages'
        self.commands = self.directory/'commands'
        self.packages.mkdir()
        self.commands.mkdir()
        self.events_file = self.directory/'events.jsonl'
        native = self.packages/'native-install.sh'
        # The wrapper invokes this file using bash, just as it does a release.
        native.write_text('#!/usr/bin/env bash\nexec '+self.shell_quote(sys.executable)+
                          ' - "$@" <<\'PYTHON\'\n'+NATIVE_FIXTURE+'\nPYTHON\n')
        content = native.read_bytes()
        (self.packages/'manifest.tsv').write_text(
            'format\toss-transfer-go-v1\nversion\t4.1.21\ncommit\t'+'a'*40+'\n'+
            'asset\tnative-install.sh\t'+hashlib.sha256(content).hexdigest()+'\t'+str(len(content))+'\n')
        self.script('fixture-cli', CLI_FIXTURE)
        self.script('curl', CURL_FIXTURE)
        self.shell_script('id', 'printf "0\\n"')
        self.shell_script('uname', 'case "$1" in -s) printf "%s\\n" "$FIXTURE_OS";; -m) printf "x86_64\\n";; esac')
        self.environment = dict(os.environ, PATH=str(self.commands)+os.pathsep+os.environ['PATH'],
                                INSTALLER_EVENTS=str(self.events_file), FIXTURE_OS='Linux',
                                FIXTURE_CLI=str(self.commands/'fixture-cli'),
                                MASTER_ORIGIN='http://127.0.0.1:1685',
                                ACCEPTED_AGENT_TOKEN='otcli1.same-cluster-token')
        self.environment.pop('TRANSFER_JOIN_TOKEN', None)
        self.environment.pop('OSS_TRANSFER_JOIN_TOKEN', None)

    @staticmethod
    def shell_quote(value):
        return "'"+value.replace("'", "'\"'\"'")+"'"

    def script(self, name, body):
        path = self.commands/name
        path.write_text('#!'+sys.executable+'\n'+body)
        path.chmod(0o755)

    def shell_script(self, name, body):
        path = self.commands/name
        path.write_text('#!/bin/sh\n'+body+'\n')
        path.chmod(0o755)

    def run_installer(self, *arguments, installer=INSTALLER, default_root=False, **environment):
        root_arguments = [] if default_root else ['--root-dir', str(self.cluster)]
        return subprocess.run(
            ['bash', str(installer), '--yes', '--lang', 'en', *root_arguments,
             '--package-dir', str(self.packages), '--cli-path', str(self.directory/'cli'), *arguments],
            env=dict(self.environment, **environment), text=True, capture_output=True, timeout=20)

    def events(self):
        return [json.loads(line) for line in self.events_file.read_text().splitlines()] if self.events_file.exists() else []

    def seed_agent(self, cluster=None, legacy=False, token='otcli1.same-cluster-token'):
        cluster = cluster or self.cluster
        install = cluster/'agent' if legacy else cluster/'agent/bin'
        # Legacy installs kept configuration/data elsewhere (for example in
        # /etc and /var/lib). Only install.tsv locates that retained identity.
        config = self.directory/'legacy-configs/agent/service.json' if legacy else cluster/'agent/config/service.json'
        data = self.directory/'legacy-agent-data' if legacy else cluster/'agent/data'
        for directory in (install, config.parent, data):
            directory.mkdir(parents=True, exist_ok=True)
        config.write_text(json.dumps({'dataDirectory': str(data)}))
        (data/'cli-local-auth.json').write_text(json.dumps({'token': token}))
        (install/'install.tsv').write_text('role\tagent\nconfig\t'+str(config)+'\ndataDir\t'+str(data)+'\nserviceName\toss-transfer-agent\n')
        return install

    def installer_with_fixture_defaults(self):
        # Redirect only system layout paths. The real wrapper still performs
        # discovery, credential verification and native argument construction.
        path = self.directory/'install-with-fixture-defaults.sh'
        source = INSTALLER.read_text().replace('/opt/oss-transfer', str(self.cluster))
        source = source.replace('/etc/oss-transfer', str(self.directory/'legacy-configs'))
        path.write_text(source)
        return path

    def assert_agent_origin(self, expected):
        agent = next(event for event in self.events() if event.get('role') == 'agent')
        self.assertEqual(agent['args'][agent['args'].index('--master-url')+1], expected)
        return agent

    def test_default_master_installs_and_joins_local_agent(self):
        result = self.run_installer('--role', 'master')
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        self.assertEqual([event['event'] for event in self.events()], ['native', 'invite', 'native'])
        agent = self.assert_agent_origin(self.environment['MASTER_ORIGIN'])
        self.assertEqual(agent['token'], 'local-agent-invitation-fixture-123456')
        self.assertNotIn(agent['token'], result.stdout+result.stderr+' '.join(agent['args']))
        self.assertEqual(agent['args'][agent['args'].index('--package-dir')+1], str(self.cluster/'master/data/packages'))

    def test_without_agent_installs_master_only(self):
        result = self.run_installer('--role', 'master', '--without-agent')
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        self.assertEqual([(event['event'], event.get('role')) for event in self.events()], [('native', 'master')])

    def test_chinese_combined_install_ends_with_access_details_once(self):
        result = self.run_installer('--role', 'master', '--lang', 'zh',
                                    MASTER_ORIGIN='http://10.0.0.2:1688', NEW_PASSWORD='new_test_password')
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        self.assertEqual([e['lang'] for e in self.events() if e['event'] == 'native'], ['zh', 'zh'])
        self.assertEqual(result.stdout.count('初始密码: new_test_password'), 1)
        self.assertIn('Master 和本机 Agent 安装成功', result.stdout)
        self.assertTrue(result.stdout.endswith('访问地址: http://10.0.0.2:1688\n初始管理员: admin\n初始密码: new_test_password\n'))
        self.assertGreater(result.stdout.index('访问地址:'), result.stdout.index('Agent installation finished'))

    def test_agent_failure_preserves_master_access_without_cluster_success(self):
        result = self.run_installer('--role', 'master', '--lang', 'zh',
                                    NEW_PASSWORD='new_test_password', AGENT_EXIT_CODE='1')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('Master 和本机 Agent 安装成功', result.stdout)
        self.assertIn('初始密码: new_test_password', result.stdout)
        self.assertIn('访问地址: http://127.0.0.1:1685', result.stdout)

    def test_chinese_reinstall_does_not_invent_a_password(self):
        result = self.run_installer('--role', 'master', '--lang', 'zh')
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        self.assertNotIn('初始密码', result.stdout)
        self.assertNotIn('初始管理员', result.stdout)
        self.assertIn('访问地址:', result.stdout)

    def test_with_agent_uses_published_ipv6_origin_not_wildcard_check_config(self):
        origin = 'http://[::1]:1688'
        result = self.run_installer('--role', 'master', '--with-agent', MASTER_ORIGIN=origin)
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        self.assert_agent_origin(origin)
        invitation = next(event for event in self.events() if event['event'] == 'invite')
        self.assertEqual(invitation['args'][invitation['args'].index('--host')+1], origin)

    def test_existing_legacy_agent_in_selected_root_reuses_verified_identity(self):
        install = self.seed_agent(legacy=True)
        result = self.run_installer('--role', 'master', '--with-agent')
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        self.assertEqual([event['event'] for event in self.events()], ['native', 'verify', 'native'])
        agent = self.assert_agent_origin(self.environment['MASTER_ORIGIN'])
        self.assertEqual(agent['args'][agent['args'].index('--install-dir')+1], str(install))
        self.assertEqual(agent['token'], '')

    def test_existing_agent_with_ipv6_master_origin_is_verified(self):
        # Local Masters usually publish http://[::1]:PORT for the CLI.
        origin = 'http://[::1]:1688'
        self.seed_agent(legacy=True)
        result = self.run_installer('--role', 'master', '--with-agent', MASTER_ORIGIN=origin)
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        self.assertEqual([event['event'] for event in self.events()], ['native', 'verify', 'native'])
        self.assert_agent_origin(origin)

    def test_foreign_legacy_agent_is_rejected_before_agent_installation(self):
        install = self.seed_agent(legacy=True, token='otcli1.other-cluster-token')
        before = (install/'install.tsv').read_bytes()
        result = self.run_installer('--role', 'master', '--with-agent')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Existing Agent belongs to another cluster', result.stderr)
        self.assertEqual([event['event'] for event in self.events()], ['native', 'verify'])
        self.assertEqual((install/'install.tsv').read_bytes(), before)
        self.assertNotIn('other-cluster-token', result.stdout+result.stderr)

    def test_removed_legacy_agent_binary_tree_reuses_config_and_identity(self):
        for master_layout in ('master', 'master/bin'):
            with self.subTest(master_layout=master_layout):
                self.cluster = self.directory/('cluster-'+master_layout.replace('/', '-'))
                install = self.seed_agent(legacy=True)
                shutil.rmtree(install)
                self.events_file.unlink(missing_ok=True)
                config = self.directory/'legacy-configs/agent/service.json'
                master_config = self.directory/'custom-master-config'
                result = self.run_installer(
                    '--role', 'master', '--with-agent', '--config-dir', str(master_config),
                    '--install-dir', str(self.cluster/master_layout),
                    installer=self.installer_with_fixture_defaults(), default_root=True,
                    DEFAULT_INSTALL_ROOT=str(self.cluster), MASTER_INSTALL_DIR=str(self.cluster/master_layout))
                self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
                self.assertEqual([event['event'] for event in self.events()], ['native', 'verify', 'native'])
                agent = self.assert_agent_origin(self.environment['MASTER_ORIGIN'])
                self.assertEqual(agent['args'][agent['args'].index('--config-dir')+1], str(config.parent))
                self.assertNotIn(str(master_config), agent['args'])
                self.assertNotIn('--root-dir', agent['args'])
                self.assertNotIn('--service-name', agent['args'])
                self.assertEqual(agent['token'], '')

    def test_removed_foreign_legacy_agent_binary_tree_is_rejected(self):
        self.cluster = self.directory/'default-cluster'
        install = self.seed_agent(legacy=True, token='otcli1.other-cluster-token')
        shutil.rmtree(install)
        config = self.directory/'legacy-configs/agent/service.json'
        before = config.read_bytes()
        result = self.run_installer(
            '--role', 'master', '--with-agent',
            installer=self.installer_with_fixture_defaults(), default_root=True,
            DEFAULT_INSTALL_ROOT=str(self.cluster))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Existing Agent belongs to another cluster', result.stderr)
        self.assertEqual([event['event'] for event in self.events()], ['native', 'verify'])
        self.assertEqual(config.read_bytes(), before)
        self.assertNotIn('other-cluster-token', result.stdout+result.stderr)

    def test_removed_agent_metadata_with_explicit_root_does_not_reuse_master_config(self):
        install = self.seed_agent()
        shutil.rmtree(install)
        master_config = self.directory/'custom-master-config'
        result = self.run_installer('--role', 'master', '--with-agent', '--config-dir', str(master_config))
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        self.assertEqual([event['event'] for event in self.events()], ['native', 'verify', 'native'])
        agent = self.assert_agent_origin(self.environment['MASTER_ORIGIN'])
        self.assertEqual(agent['args'][agent['args'].index('--config-dir')+1], str(self.cluster/'agent/config'))
        self.assertEqual(agent['args'][agent['args'].index('--root-dir')+1], str(self.cluster))
        self.assertNotIn(str(master_config), agent['args'])

    def test_explicit_root_does_not_discover_an_agent_from_default_macos_root(self):
        home = self.directory/'home'
        other = home/'Library/Application Support/OSS Transfer'
        self.seed_agent(cluster=other, token='otcli1.other-cluster-token')
        result = self.run_installer('--role', 'master', '--with-agent', '--user',
                                    HOME=str(home), FIXTURE_OS='Darwin')
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        self.assertEqual([event['event'] for event in self.events()], ['native', 'invite', 'native'])
        self.assertIn('--user', self.assert_agent_origin(self.environment['MASTER_ORIGIN'])['args'])

    def test_freebsd_local_agent_uses_valid_service_name(self):
        result = self.run_installer('--role', 'master', '--with-agent', FIXTURE_OS='FreeBSD')
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        agent = self.assert_agent_origin(self.environment['MASTER_ORIGIN'])
        self.assertEqual(agent['args'][agent['args'].index('--service-name')+1], 'oss_transfer_agent')

    def test_same_version_rerun_ends_with_nothing_changed(self):
        self.seed_agent()
        result = self.run_installer('--role', 'master', '--with-agent', '--lang', 'zh', NATIVE_RESULT='unchanged')
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        self.assertIn('正在检查本机 Agent…', result.stdout)
        self.assertNotIn('正在安装本机 Agent', result.stdout)
        self.assertNotIn('安装成功', result.stdout)
        self.assertNotIn('已加入集群', result.stdout)
        self.assertTrue(result.stdout.rstrip().endswith('=== 无需更改 ==='), result.stdout)
        # Tooling still finds the installation lines.
        self.assertIn('Installation: ', result.stdout)
        result = self.run_installer('--role', 'master', '--without-agent', NATIVE_RESULT='unchanged')
        self.assertTrue(result.stdout.rstrip().endswith('=== Nothing changed ==='), result.stdout)

    def test_changed_installation_keeps_success_banner(self):
        self.seed_agent()
        result = self.run_installer('--role', 'master', '--with-agent', NATIVE_RESULT='installed')
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        self.assertIn('Master and local Agent installed successfully', result.stdout)
        self.assertNotIn('Nothing changed', result.stdout)
        result = self.run_installer('--role', 'master', '--without-agent')
        self.assertNotIn('Nothing changed', result.stdout)

    def keep_agent_data(self, version):
        install = self.seed_agent()
        config = self.cluster/'agent/config'
        shutil.rmtree(install)
        (config/'installed-version').write_text(version+'\n')
        return config

    def test_older_release_over_kept_data_needs_force(self):
        # The release fixture is 4.1.21; the kept data was last used by a prerelease of 4.2.0.
        self.keep_agent_data('4.2.0-rc.1')
        result = self.run_installer('--role', 'agent')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('the agent data was last used by 4.2.0-rc.1; 4.1.21 is older', result.stderr)
        self.assertIn('Add --version 4.2.0-rc.1', result.stderr)
        self.assertEqual(self.events(), [])
        result = self.run_installer('--role', 'agent', '--force')
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        self.assertIn('--force', self.events()[-1]['args'])

    def test_kept_data_from_the_same_or_older_release_installs_normally(self):
        for version in ('4.1.21', '4.1.20'):
            with self.subTest(version=version):
                self.events_file.unlink(missing_ok=True)
                self.keep_agent_data(version)
                result = self.run_installer('--role', 'agent')
                self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
                self.assertNotIn('older', result.stderr)
                self.assertNotIn('--force', self.events()[-1]['args'])

    def test_older_release_for_kept_local_agent_data_forces_only_after_confirmation(self):
        self.keep_agent_data('4.2.0')
        result = self.run_installer('--role', 'master', '--with-agent')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('the agent data was last used by 4.2.0', result.stderr)
        self.assertEqual(self.events(), [])
        result = self.run_installer('--role', 'master', '--with-agent', '--force', NEW_PASSWORD='new_test_password')
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        self.assertIn('--force', [event for event in self.events() if event.get('role') == 'agent'][-1]['args'])

    def plan(self, role='master', with_agent='1', **variables):
        source = INSTALLER.read_text()
        wrapper = source[source.index('# END embedded native installer\n'):]
        functions = ''
        for name in ('say', 'default_install_dir', 'existing_install_dir', 'retained_role_config', 'recorded_version',
                     'release_kind', 'print_native_plan'):
            start = wrapper.index('\n'+name+'() {')+1
            end = wrapper.index('\n}\n', start) if not wrapper[start:wrapper.index('\n', start)].endswith('}') else wrapper.index('\n', start)
            functions += wrapper[start:end+1] + ('}\n' if wrapper[end:end+3] == '\n}\n' else '\n')
        functions += source[source.index('transfer_version_older() {'):source.index('\n}\n', source.index('transfer_version_older() {'))+3]
        values = dict(lang='en', role=role, with_agent=with_agent, install_dir='', data_dir='', service_name='', config_dir='',
                      root_dir=str(self.cluster), os='Linux', user_service='0', listen='', port='1685', adopt_config='',
                      version='4.2.0-rc.1')
        values.update(variables)
        script = 'set -euo pipefail\n' + functions + ''.join(f'{key}={self.shell_quote(value)}\n' for key, value in values.items()) + 'print_native_plan\n'
        result = subprocess.run(['bash', '-c', script], text=True, capture_output=True, timeout=20)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout

    def test_plan_lists_roles_prerelease_directories_and_service(self):
        plan = self.plan()
        self.assertEqual(plan, 'Installation plan:\n  Role: Master + local Agent\n  Version: 4.2.0-rc.1 (prerelease)\n'
                               f'  Programs: {self.cluster}/master/bin\n  Data: {self.cluster}/master/data\n'
                               '  Service: oss-transfer-master\n  Listen: 0.0.0.0:1685\n')
        plan = self.plan(role='agent', with_agent='0', version='4.2.0', lang='zh')
        self.assertIn('角色: 执行节点\n  版本: 4.2.0\n', plan)
        self.assertNotIn('预发布', plan)
        self.assertNotIn('监听地址', plan)

    def test_plan_shows_the_kept_installation_it_replaces(self):
        config = self.keep_agent_data('4.1.41')
        (config/'service.json').write_text(json.dumps({'serviceName': 'edge-agent', 'dataDirectory': '/srv/agent-data'}))
        plan = self.plan(role='agent', with_agent='0')
        self.assertIn('Version: 4.2.0-rc.1 (prerelease), replacing 4.1.41\n', plan)
        self.assertIn('Data: /srv/agent-data\n', plan)
        self.assertIn('Service: edge-agent\n', plan)

    def test_with_agent_rejects_agent_role_before_native_installation(self):
        result = self.run_installer('--role', 'agent', '--with-agent')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('--with-agent requires a native Master installation', result.stderr)
        self.assertEqual(self.events(), [])


if __name__ == '__main__':
    unittest.main()
