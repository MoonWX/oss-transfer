#!/usr/bin/env python3
"""Real native service install/upgrade/rollback in an isolated service namespace.

Linux requires root + systemd; FreeBSD requires root + rc.d; Windows requires
Administrator; macOS uses an isolated user LaunchAgent. Never uses default names.
"""
import argparse
from contextlib import contextmanager
import hashlib
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import json
import os
from pathlib import Path
import platform
import re
import secrets
import shlex
import shutil
import socket
import subprocess
import tarfile
import threading
import time
import zipfile
from urllib.request import Request, urlopen


def require(condition, message):
    if not condition:
        raise AssertionError(message)


PREPARE_SPEC = importlib.util.spec_from_file_location('prepare_go', Path(__file__).with_name('prepare.py'))
PREPARE = importlib.util.module_from_spec(PREPARE_SPEC)
PREPARE_SPEC.loader.exec_module(PREPARE)


class Lifecycle:
    def __init__(self, args):
        self.args = args
        self.root = args.output.resolve()
        self.system = platform.system()
        self.os_name = {'Linux':'linux','Darwin':'darwin','Windows':'windows','FreeBSD':'freebsd'}[self.system]
        machine = platform.machine().lower()
        host_arch = {'x86_64':'amd64','amd64':'amd64','arm64':'arm64','aarch64':'arm64','armv7':'armv7','armv7l':'armv7','riscv64':'riscv64','loongarch64':'loong64','loong64':'loong64'}[machine]
        self.arch = host_arch
        if args.target_platform:
            target_os, self.arch = args.target_platform.split('/')
            require(target_os == self.os_name, 'Target OS must match the service manager host')
            require(self.arch == host_arch or self.system == 'Linux',
                    'Cross-architecture service acceptance requires Linux binfmt support')
        require(bool(args.failure_master_fixture) == bool(args.rollback_cli_fixture),
                'Precompiled failure Master and rollback CLI fixtures must be supplied together')
        # Reject a host binary mislabeled as the target before running it or
        # creating any service files. The same check covers precompiled fixtures.
        for source in (args.master, args.agent, args.cli, args.failure_master_fixture, args.rollback_cli_fixture):
            if source is not None:
                PREPARE.verify_binary(source.resolve(), self.os_name, self.arch)
        self.root.mkdir(parents=True, exist_ok=False)
        self.installer_env = dict(os.environ,
                                  QA_REAL_MASTER=str(args.master.resolve()),
                                  QA_REAL_CLI=str(args.cli.resolve()))
        if self.arch != host_arch:
            shim_dir = self.root/'installer-platform'
            shim_dir.mkdir()
            uname = shutil.which('uname')
            require(uname is not None, 'Host uname is required')
            machine_name = {'armv7':'armv7l','loong64':'loongarch64'}.get(self.arch,self.arch)
            shim = shim_dir/'uname'
            shim.write_text('#!/bin/sh\nif [ "$#" -eq 1 ] && [ "$1" = "-m" ]; then\n'
                            + "  printf '%s\\n' " + shlex.quote(machine_name) + '\nelse\n  exec '
                            + shlex.quote(uname) + ' "$@"\nfi\n', encoding='utf-8')
            shim.chmod(0o755)
            # Only the installer sees this CPU selector. systemctl and the
            # generated ExecStart remain real host services using target ELF.
            self.installer_env['PATH'] = str(shim_dir)+os.pathsep+os.environ.get('PATH','')
        self.cli_path=self.root/'shared CLI'/('oss-transfer-cli.exe' if self.system=='Windows' else 'oss-transfer-cli')
        self.names = {role:'oss-qa-'+role+'-'+secrets.token_hex(5) for role in ('master','agent')}
        if self.system == 'FreeBSD': self.names = {role:name.replace('-','_') for role,name in self.names.items()}
        self.roles = []
        self.secrets = []
        self.token = None
        self.evidence = {'platform':self.os_name+'/'+self.arch,
                         'hostPlatform':self.os_name+'/'+host_arch,
                         'crossArchitecture':self.arch != host_arch,'checks':[]}
        if self.arch != host_arch:
            self.evidence['limitations'] = ['Target CPU binaries execute through host Linux binfmt; kernel and systemd remain the host platform']
        self.release = self.root/'release'
        self.release.mkdir()
        self.payloads = {role:getattr(args,role).resolve() for role in ('master','agent','cli')}
        rows = ['format\toss-transfer-go-v1']
        version = None
        for role in ('master','agent','cli'):
            source = getattr(args,role).resolve()
            output = subprocess.check_output([str(source),'--version'],text=True, encoding="utf-8").strip()
            current = json.loads(subprocess.check_output([str(source),'--version','--json'],text=True,encoding='utf-8'))['version'] if role=='cli' else output.split()[1]
            require(version is None or version == current,'Master and Agent versions differ')
            version = current
            self.evidence[role] = {'version':output,'sha256':hashlib.sha256(source.read_bytes()).hexdigest()}
            rows.append(self.row(role,source))
        if self.system!='Windows':
            native=self.release/'native-install.sh'
            shutil.copy2(args.repo/'deploy/native/install.sh',native)
            payload=native.read_bytes()
            rows.append('\t'.join(('asset',native.name,hashlib.sha256(payload).hexdigest(),str(len(payload)))))
        self.version = version
        rows[1:1] = ['version\t'+version,'commit\tisolated-lifecycle-test']
        (self.release/'manifest.tsv').write_text('\n'.join(rows)+'\n', encoding="utf-8")
        self.write_archive(self.release,self.payloads)
        with socket.socket() as listener:
            listener.bind(('127.0.0.1',0))
            self.port = listener.getsockname()[1]
        self.origin = 'http://127.0.0.1:'+str(self.port)
        self.save()

    def row(self,role,path):
        raw=path.read_bytes()
        return '\t'.join(('file',self.os_name,self.arch,role,self.binary_name(role),hashlib.sha256(raw).hexdigest(),str(len(raw))))

    def binary_name(self,role):
        return f'oss-transfer-{role}-{self.os_name}-{self.arch}'+('.exe' if self.system=='Windows' else '')

    def write_archive(self,directory,payloads,update_file_rows=True):
        prefix=f'oss-transfer-{self.os_name}-{self.arch}'
        suffix='.zip' if self.system=='Windows' else '.tar.gz'
        archive=directory/(prefix+suffix)
        if self.system=='Windows':
            with zipfile.ZipFile(archive,'w',compression=zipfile.ZIP_DEFLATED,compresslevel=1) as target:
                for role,source in payloads.items():
                    target.write(source,prefix+'/oss-transfer-'+role+'.exe')
        else:
            with tarfile.open(archive,'w:gz',compresslevel=1) as target:
                for role,source in payloads.items():
                    target.add(source,arcname=prefix+'/oss-transfer-'+role,recursive=False)
        manifest=directory/'manifest.tsv'
        rows=[]
        for line in manifest.read_text(encoding='utf-8').splitlines():
            fields=line.split('\t')
            if fields[0]=='archive':continue
            if update_file_rows and fields[0]=='file':line=self.row(fields[3],payloads[fields[3]])
            rows.append(line)
        rows.append('\t'.join(('archive',self.os_name,self.arch,archive.name,hashlib.sha256(archive.read_bytes()).hexdigest(),str(archive.stat().st_size))))
        manifest.write_text('\n'.join(rows)+'\n',encoding='utf-8')
        return archive

    def legacy_release(self):
        directory=self.root/'legacy-release'
        shutil.copytree(self.release,directory)
        for entry in directory.iterdir():
            if entry.name.endswith(('.zip','.tar.gz')):entry.unlink()
        for role,source in self.payloads.items():shutil.copy2(source,directory/self.binary_name(role))
        manifest=directory/'manifest.tsv'
        manifest.write_text('\n'.join(line for line in manifest.read_text(encoding='utf-8').splitlines() if not line.startswith('archive\t'))+'\n',encoding='utf-8')
        return directory

    def scrub(self,text):
        text=re.sub(r'((?:Initial password|Setup token):) \S+',r'\1 [redacted]',text)
        for secret in self.secrets:
            text=text.replace(secret,'[redacted]')
        return text

    def save(self):
        (self.root/'evidence.json').write_text(self.scrub(json.dumps(self.evidence,indent=2))+'\n', encoding="utf-8")

    def record(self,name,**details):
        self.evidence['checks'].append(dict(name=name,**details))
        self.save()
        print('PASS '+name,flush=True)

    def invoke(self,args,**kwargs):
        result=subprocess.run([str(arg) for arg in args],stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True,timeout=kwargs.pop('timeout',180),**kwargs, encoding="utf-8", errors="replace")
        return result

    @contextmanager
    def latest_release(self):
        mirror=self.root/('http-release-'+secrets.token_hex(3))
        latest=mirror/'latest'
        version_name=self.version if self.version.startswith('v') else 'v'+self.version
        versioned=mirror/version_name
        latest.mkdir(parents=True)
        versioned.mkdir()
        shutil.copy2(self.release/'manifest.tsv',latest/'manifest.tsv')
        for entry in self.release.iterdir():
            os.link(entry,versioned/entry.name)
        requests=[]
        class Handler(SimpleHTTPRequestHandler):
            def __init__(self,*args,**kwargs): super().__init__(*args,directory=str(mirror),**kwargs)
            def do_GET(self):
                requests.append(self.path)
                return super().do_GET()
            def log_message(self,*args): pass
        server=ThreadingHTTPServer(('127.0.0.1',0),Handler)
        thread=threading.Thread(target=server.serve_forever,daemon=True)
        thread.start()
        try:
            yield 'http://127.0.0.1:'+str(server.server_port)+'/latest'
            require('/latest/manifest.tsv' in requests and '/'+version_name+'/manifest.tsv' in requests,
                    'Latest installation did not resolve the fixed-version manifest')
            require(requests.count('/latest/manifest.tsv') == 1, 'Latest was resolved separately for installed roles')
            require(not any(path.startswith('/latest/') and path!='/latest/manifest.tsv' for path in requests),
                    'Installer downloaded a mutable latest binary')
            archive_suffix='.zip' if self.system=='Windows' else '.tar.gz'
            archive_name=f'oss-transfer-{self.os_name}-{self.arch}'+archive_suffix
            allowed={'/latest/manifest.tsv','/'+version_name+'/manifest.tsv','/'+version_name+'/'+archive_name}
            if self.system!='Windows':allowed.add('/'+version_name+'/native-install.sh')
            require(set(requests)<=allowed and '/'+version_name+'/'+archive_name in requests,
                    'Installer did not use the verified archive, or requested a loose binary')
            self.record('latest_resolves_fixed_version_before_binary_download',requests=requests)
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)

    def install(self,role,upgrade=False,release=None,release_url=None,expect=0,token=None,timeout=120,adopt_config=None,replace_cli=False,discover=False,discover_all=False):
        directory=self.root/role
        paths={name:directory/name for name in ('bin','config','data','backups')}
        if role not in self.roles:
            self.roles.append(role)
        if self.system=='Windows':
            command=['powershell.exe','-NoProfile','-ExecutionPolicy','Bypass','-File',self.args.repo/'deploy/native/Install.ps1',
                     '-Role',role,('-ReleaseUrl' if release_url else '-PackageDir'),release_url or release or self.release,'-ServiceName',self.names[role],'-InstallDir',paths['bin'],
                     '-ConfigDir',paths['config'],'-DataDir',paths['data'],'-BackupDir',paths['backups'],'-Yes','-StartupTimeout',str(timeout)]
            if upgrade: command+=['-Upgrade']
            if adopt_config: command+=['-AdoptConfig',adopt_config]
            if role=='master':command+=['-CliPath',self.cli_path]
            if replace_cli:command+=['-ReplaceCli']
            if role=='master':command+=['-Listen','127.0.0.1:'+str(self.port)]
            else:command+=['-MasterUrl',self.origin]
        else:
            command=['bash',self.args.repo/'install.sh','--role',role,('--release-url' if release_url else '--package-dir'),release_url or release or self.release,
                     '--service-name',self.names[role],'--install-dir',paths['bin'],'--config-dir',paths['config'],
                     '--data-dir',paths['data'],'--backup-dir',paths['backups'],'--yes','--startup-timeout',str(timeout)]
            if self.system=='Darwin' and not self.args.system:command+=['--user']
            if upgrade:command+=['--upgrade']
            if adopt_config:command+=['--adopt-config',adopt_config]
            if role=='master':command+=['--cli-path',self.cli_path]
            if replace_cli:command+=['--replace-cli']
            if role=='master':command+=['--listen','127.0.0.1:'+str(self.port)]
            else:command+=['--master-url',self.origin]
        if discover:
            require(upgrade, 'Discovery is an upgrade operation')
            # A custom service-name filter isolates this test on shared runners.
            # Role, all directories, CLI path and enrollment inputs are omitted.
            if self.system == 'Windows':
                command=['powershell.exe','-NoProfile','-ExecutionPolicy','Bypass','-File',self.args.repo/'deploy/native/Install.ps1',
                         '-Upgrade',('-ReleaseUrl' if release_url else '-PackageDir'),release_url or release or self.release,'-Yes','-StartupTimeout',str(timeout)]
                if not discover_all: command+=['-ServiceName',self.names[role]]
            else:
                command=['bash',self.args.repo/'install.sh','--upgrade',('--release-url' if release_url else '--package-dir'),release_url or release or self.release,
                         '--yes','--startup-timeout',str(timeout)]
                if not discover_all: command+=['--service-name',self.names[role]]
                if self.system == 'Darwin' and not self.args.system: command+=['--user']
        observations=[]
        monitor_stop=threading.Event()
        def monitor_scm():
            for _ in range(4):
                if monitor_stop.wait(10): return
                try:
                    result=self.invoke(['powershell.exe','-NoProfile','-Command',
                        "$ErrorActionPreference='Stop'; @{service=(Get-CimInstance Win32_Service -Filter \"Name='"+self.names[role]+"'\" -ErrorAction SilentlyContinue | Select-Object State,ProcessId,ExitCode); processes=@(Get-Process -Name 'oss-transfer-"+role+"' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq '"+str(paths['bin']/f'oss-transfer-{role}.exe').replace("'","''")+"' } | Select-Object Id,Path,CPU,StartTime,WorkingSet64)} | ConvertTo-Json -Depth 4"],timeout=8)
                    observations.append(result.stdout)
                except Exception as error: observations.append('SCM observation unavailable: '+str(error))
        monitor=threading.Thread(target=monitor_scm,daemon=True) if self.system=='Windows' else None
        if monitor:monitor.start()
        try: result=self.invoke(command,env=dict(self.installer_env,TRANSFER_JOIN_TOKEN=token or ''),timeout=timeout+100)
        finally:
            monitor_stop.set()
            if monitor:monitor.join(timeout=10)
        log=self.root/f'{len(list(self.root.glob("*-install-*.log"))):02d}-install-{role}.log'
        log.write_text(self.scrub(result.stdout+'\nSCM startup observations:\n'+'\n'.join(observations) if observations else result.stdout), encoding="utf-8")
        if (result.returncode==0)!=(expect==0):
            # An installer can fail before our test reads its initial password.
            # Preserve the service's own error with credential redaction.
            service_log=paths['data']/'logs/service.log'
            try:
                if service_log.is_file():
                    with log.open('a',encoding='utf-8') as output:
                        output.write('\nService log:\n'+self.scrub(service_log.read_text(encoding='utf-8',errors='replace')))
            except OSError:
                print('Service log could not be read; original installer failure retained.',flush=True)
            if self.system=='Windows':
                try: self.windows_install_diagnostics(role,paths,log,upgrade)
                except Exception:
                    print('Additional Windows diagnostics failed; original installer failure retained.',flush=True)
        require((result.returncode==0)==(expect==0),'Installer result unexpected; see '+str(log))
        if expect==0 and role=='master':
            require('Login: ' in result.stdout and '--host '+("'" if self.system=='Windows' else '')+self.origin in result.stdout,
                    'Installer did not print a CLI login command for the configured listener')
            if hasattr(self, 'credentials'):
                require('Initial password:' not in result.stdout,
                        'Installer reprinted an initial password from an existing installation')
        return result

    def windows_install_diagnostics(self,role,paths,log,upgrade):
        # Each diagnostic is best effort. In particular, a slow event-log query
        # must not replace the installer failure or prevent the direct probe.
        def append(title,text):
            with log.open('a',encoding='utf-8') as output:
                output.write('\n'+title+':\n'+self.scrub(text)+'\n')
        service_log=paths['data']/'logs/service.log'
        if not upgrade and not service_log.is_file():
            probe=None
            try:
                probe=subprocess.Popen([str(paths['bin']/f'oss-transfer-{role}.exe'),'--config',str(paths['config']/'service.json')],
                                       stdout=subprocess.PIPE,stderr=subprocess.STDOUT)
                try:
                    raw,_=probe.communicate(timeout=3)
                    summary='Foreground exit: '+str(probe.returncode)
                except subprocess.TimeoutExpired:
                    probe.terminate()
                    try: raw,_=probe.communicate(timeout=5)
                    except subprocess.TimeoutExpired:
                        probe.kill()
                        raw,_=probe.communicate(timeout=5)
                    summary='Foreground process remained running for 3 seconds; probe stopped.'
                append(summary,raw.decode('utf-8',errors='replace'))
            except Exception as error:
                append('Foreground probe unavailable',str(error))
            finally:
                if probe is not None and probe.poll() is None:
                    probe.kill()
                    try: probe.communicate(timeout=5)
                    except subprocess.TimeoutExpired: append('Foreground cleanup failed','Process did not exit after kill')
            try:
                if service_log.is_file(): append('Service log after foreground probe',service_log.read_text(encoding='utf-8',errors='replace'))
            except OSError as error: append('Cannot read service log',str(error))
        try:
            events=self.invoke(['powershell.exe','-NoProfile','-Command',
                "$ErrorActionPreference='Stop'; Get-WinEvent -MaxEvents 32 -FilterHashtable @{LogName='System'; ProviderName='Service Control Manager'; StartTime=(Get-Date).AddMinutes(-5)} | Where-Object { $_.Message -like '*"+self.names[role]+"*' -or $_.Message -like '*OSS Transfer Go "+role+"*' } | Select-Object -First 8 Id,Message | ConvertTo-Json -Depth 3"],timeout=10)
            append('Service manager events',events.stdout)
        except subprocess.TimeoutExpired:
            append('Service manager events unavailable','Query exceeded 10 seconds; original installer failure retained.')
        except Exception as error:
            append('Service manager events unavailable',str(error))
        if not upgrade and role=='master':
            self.windows_context_diagnostics(paths,append)

    def windows_context_diagnostics(self,paths,append):
        def quote(value): return "'"+str(value).replace("'","''")+"'"
        # Run the same foreground binary as SYSTEM. A scheduled task is used only
        # inside this disposable QA installation and is always removed afterward.
        name=self.names['master']+'-system-probe'
        probe_script=self.root/'system-foreground.ps1'
        probe_stdout=self.root/'system-foreground.stdout'
        probe_stderr=self.root/'system-foreground.stderr'
        probe_trace=self.root/'system-foreground.trace'
        check_stdout=self.root/'system-config.stdout'
        check_stderr=self.root/'system-config.stderr'
        probe_script.write_text("$ErrorActionPreference='Stop'; try { \"identity=$([Security.Principal.WindowsIdentity]::GetCurrent().Name) cwd=$([Environment]::CurrentDirectory) powershell=$((Get-Location).Path)\" | Out-File -Encoding utf8 -LiteralPath "+quote(probe_trace)+"; "
            "$check=Start-Process -FilePath "+quote(paths['bin']/'oss-transfer-master.exe')+" -ArgumentList "+quote('--config "'+str(paths['config']/'service.json')+'" --check-config')+
            " -RedirectStandardOutput "+quote(check_stdout)+" -RedirectStandardError "+quote(check_stderr)+" -PassThru -Wait; \"check-config exit=$($check.ExitCode)\" | Out-File -Encoding utf8 -Append -LiteralPath "+quote(probe_trace)+"; "
            "$process=Start-Process -FilePath "+quote(paths['bin']/'oss-transfer-master.exe')+" -ArgumentList "+quote('--config "'+str(paths['config']/'service.json')+'"')+
            " -RedirectStandardOutput "+quote(probe_stdout)+" -RedirectStandardError "+quote(probe_stderr)+" -PassThru -Wait; \"exit=$($process.ExitCode)\" | Out-File -Encoding utf8 -Append -LiteralPath "+quote(probe_trace)+
            " } catch { $_ | Out-String | Out-File -Encoding utf8 -Append -LiteralPath "+quote(probe_trace)+"; exit 1 }",encoding='utf-8')
        scheduled=("$ErrorActionPreference='Stop'; $name="+quote(name)+"; try { "
                   "$action=New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "+quote('-NoProfile -ExecutionPolicy Bypass -File "'+str(probe_script)+'"')+"; "
                   "$principal=New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest; "
                   "Register-ScheduledTask -TaskName $name -Action $action -Principal $principal | Out-Null; "
                   "Start-ScheduledTask -TaskName $name; Start-Sleep -Seconds 8; "
                   "Get-ScheduledTaskInfo -TaskName $name | Select-Object LastTaskResult,LastRunTime | ConvertTo-Json; "
                   "Get-Process -Name 'oss-transfer-master' -ErrorAction SilentlyContinue | Where-Object {$_.Path -eq "+quote(paths['bin']/'oss-transfer-master.exe')+"} | Select-Object Id,Path,CPU | ConvertTo-Json "
                   "} finally { Stop-ScheduledTask -TaskName $name -ErrorAction SilentlyContinue; "
                   "Unregister-ScheduledTask -TaskName $name -Confirm:$false -ErrorAction SilentlyContinue }")
        try:
            result=self.invoke(['powershell.exe','-NoProfile','-Command',scheduled],timeout=40)
            append('SYSTEM foreground scheduled task',result.stdout)
            for captured in (probe_trace,check_stdout,check_stderr,probe_stdout,probe_stderr):
                if captured.is_file(): append(captured.name,captured.read_text(encoding='utf-8-sig',errors='replace').replace('\x00',''))
            log=paths['data']/'logs/service.log'
            if log.is_file(): append('Service log after SYSTEM foreground',log.read_text(encoding='utf-8',errors='replace'))
        except Exception as error: append('SYSTEM foreground diagnostic unavailable',str(error))
        finally:
            try:
                cleanup="$ErrorActionPreference='Continue'; Stop-ScheduledTask -TaskName "+quote(name)+" -ErrorAction SilentlyContinue; Unregister-ScheduledTask -TaskName "+quote(name)+" -Confirm:$false -ErrorAction SilentlyContinue; Get-Process -Name 'oss-transfer-master' -ErrorAction SilentlyContinue | Where-Object {$_.Path -eq "+quote(paths['bin']/'oss-transfer-master.exe')+"} | Stop-Process -Force"
                self.invoke(['powershell.exe','-NoProfile','-Command',cleanup],timeout=15)
            except Exception as error: append('SYSTEM foreground cleanup failed',str(error))
        directory=self.root/'.servicehost-probe'
        directory.mkdir(exist_ok=True)
        source=directory/'main.go'
        source.write_text(r"""//go:build ignore

package main
import("context";"flag";"fmt";"os";"time";"github.com/MoonWX/oss-transfer/internal/servicehost")
func main(){
 cpath:=flag.String("config","","config"); trace:=flag.String("trace","","trace"); flag.Parse()
 mark:=func(s string){f,e:=os.OpenFile(*trace,os.O_CREATE|os.O_WRONLY|os.O_APPEND,0600);if e==nil{fmt.Fprintln(f,time.Now().UTC().Format(time.RFC3339Nano),s);f.Close()}}
 mark("main reached")
 c,e:=servicehost.LoadConfig(*cpath,"master");if e!=nil{mark("LoadConfig failed: "+e.Error());os.Exit(1)};mark("configuration loaded")
 e=servicehost.Run(servicehost.Options{Name:c.ServiceName,Role:"master",Version:"scm-probe",ReadyFile:c.ReadyFile,LogFile:c.LogFile},func(ctx context.Context,ready func()error)error{mark("program invoked");if e:=ready();e!=nil{return e};mark("ready");<-ctx.Done();return ctx.Err()})
 if e!=nil{mark("Run failed: "+e.Error());os.Exit(1)};mark("Run returned")
}
""",encoding='utf-8')
        binary=directory/'servicehost-probe.exe'
        mini_name=self.names['master']+'-mini'
        data=paths['data']
        config=directory/'service.json'
        trace=directory/'trace.log'
        config.write_text(json.dumps({'serviceName':mini_name,'dataDirectory':str(data),'listen':'127.0.0.1:'+str(self.port),
                                     'readyFile':str(data/'runtime/ready'),'logFile':str(data/'logs/service.log')}),encoding='utf-8')
        try:
            result=self.invoke(['go','build','-o',binary,source],cwd=self.args.repo,timeout=90)
            append('Minimal servicehost compilation',result.stdout)
            require(result.returncode==0,'Minimal servicehost could not compile')
            command='"'+str(binary)+'" --config "'+str(config)+'" --trace "'+str(trace)+'"'
            script=("$ErrorActionPreference='Stop'; try { New-Service -Name "+quote(mini_name)+" -BinaryPathName "+quote(command)+" -StartupType Manual | Out-Null; Start-Service -Name "+quote(mini_name)+"; Start-Sleep -Seconds 2; Get-CimInstance Win32_Service -Filter "+quote("Name='"+mini_name+"'")+" | Select-Object State,ProcessId,ExitCode | ConvertTo-Json } finally { Stop-Service -Name "+quote(mini_name)+" -ErrorAction SilentlyContinue; sc.exe delete "+quote(mini_name)+" }")
            result=self.invoke(['powershell.exe','-NoProfile','-Command',script],timeout=145)
            append('Minimal servicehost SCM result',result.stdout)
        except Exception as error: append('Minimal servicehost diagnostic unavailable',str(error))
        finally:
            try:
                self.invoke(['powershell.exe','-NoProfile','-Command',"Stop-Service -Name "+quote(mini_name)+" -ErrorAction SilentlyContinue; sc.exe delete "+quote(mini_name)],timeout=20)
            except Exception as error: append('Minimal servicehost cleanup failed',str(error))
            if trace.is_file(): append('Minimal servicehost trace',trace.read_text(encoding='utf-8',errors='replace'))
            else: append('Minimal servicehost trace','No entry trace file was created')
            log=data/'logs/service.log'
            if log.is_file(): append('Minimal servicehost log',log.read_text(encoding='utf-8',errors='replace'))

    def pid(self,role):
        name=self.names[role]
        if self.system=='Linux':
            return int(subprocess.check_output(['systemctl','show',name,'-p','MainPID','--value'],text=True, encoding="utf-8").strip())
        if self.system=='FreeBSD':
            return int((Path('/var/run')/(name+'.pid')).read_text().strip())
        if self.system=='Darwin':
            domain='system' if self.args.system else 'gui/'+str(os.getuid())
            output=subprocess.check_output(['launchctl','print',domain+'/'+name],text=True, encoding="utf-8")
            return int(re.search(r'\bpid = (\d+)',output)[1])
        return int(subprocess.check_output(['powershell.exe','-NoProfile','-Command',f'(Get-CimInstance Win32_Service -Filter "Name=\'{name}\'").ProcessId'],text=True, encoding="utf-8").strip())

    def api(self,path,body=None):
        headers={'Content-Type':'application/json'}
        if self.token:headers['Authorization']='Bearer '+self.token
        request=Request(self.origin+path,data=json.dumps(body).encode() if body is not None else None,headers=headers)
        with urlopen(request,timeout=15) as response:
            payload=json.load(response)
        require(payload.get('success') is not False,'API returned failure')
        return payload.get('data',payload)

    def login(self):
        self.token=self.api('/api/auth/login',self.credentials)['token']
        self.secrets.append(self.token)

    def failure_release(self):
        directory=self.root/'startup-failure'
        shutil.copytree(self.release,directory)
        fixture_directory=self.root/'failure-payloads'
        fixture_directory.mkdir()
        target=fixture_directory/self.binary_name('master')
        # A deliberately failing service fixture delegates only the real binary's
        # preflight flags. This tests rollback after stop, not a checksum refusal.
        cli_target=fixture_directory/self.binary_name('cli')
        fixture_sources = self.args.repo/'.github/acceptance/fixtures'
        build_env = dict(os.environ, CGO_ENABLED='0', GOOS=self.os_name,
                         GOARCH='arm' if self.arch == 'armv7' else self.arch)
        if self.arch == 'armv7': build_env['GOARM']='7'
        for kind, supplied, destination in (
                ('failure-master', self.args.failure_master_fixture, target),
                ('rollback-cli', self.args.rollback_cli_fixture, cli_target)):
            if supplied:
                shutil.copy2(supplied.resolve(), destination)
            else:
                result=self.invoke(['go','build','-o',destination,fixture_sources/(kind+'.go')],
                                   env=build_env,timeout=180)
                require(result.returncode==0,'Could not compile '+kind+' fixture: '+result.stdout)
            destination.chmod(0o755)
            PREPARE.verify_binary(destination, self.os_name, self.arch)
        self.evidence['failureFixtures'] = {
            'precompiled': bool(self.args.failure_master_fixture),
            'masterSha256': hashlib.sha256(target.read_bytes()).hexdigest(),
            'cliSha256': hashlib.sha256(cli_target.read_bytes()).hexdigest()}
        self.save()
        self.write_archive(directory,dict(self.payloads,master=target,cli=cli_target))
        return directory

    def run(self):
        self.cli_path.parent.mkdir()
        old_cli=b'Existing unmanaged CLI: keep until explicit replacement.\n'
        self.cli_path.write_bytes(old_cli)
        self.install('master',expect=1)
        require(self.cli_path.read_bytes()==old_cli,'Refusal replaced the existing CLI')
        with self.latest_release() as release_url:
            installed = self.install('master',release_url=release_url,replace_cli=True)
        backups=list((self.root/'master/backups').glob('*/'+('cli.exe' if self.system=='Windows' else 'cli')))
        require(len(backups)==1 and backups[0].read_bytes()==old_cli,'Existing CLI was not backed up before replacement')
        self.record('existing_cli_requires_explicit_replacement_and_has_exact_backup')
        before=self.pid('master')
        require(before>0,'Master has no service-owned PID')
        ready=self.root/'master/data/runtime/ready'
        require(ready.with_name('ready.request').read_text()==ready.with_name('ready.committed').read_text(),
                'Successful installation did not commit its startup for scan cleanup')
        printed_passwords = re.findall(r'^Initial password: ([A-Za-z0-9_-]+)$', installed.stdout, re.MULTILINE)
        require(len(printed_passwords) == 1, 'Fresh installation did not print exactly one initial password to the terminal')
        initial_password = printed_passwords[0]
        self.secrets.append(initial_password)
        self.credentials={'username':'admin','password':secrets.token_urlsafe(28)}
        self.secrets.append(self.credentials['password'])
        login=self.api('/api/auth/login',dict(self.credentials,password=initial_password))
        require(login['user']['mustChangePassword'],'Initial password did not require replacement')
        self.token=login['token']
        self.secrets.append(self.token)
        changed=self.api('/api/auth/change-password',{'oldPassword':initial_password,'newPassword':self.credentials['password']})
        require(not changed['user']['mustChangePassword'],'Password change did not unlock the account')
        self.record('fresh_master_service_initial_password_from_terminal_and_required_password_change',pid=before)
        cli_path=self.cli_path
        cli_hash=hashlib.sha256(cli_path.read_bytes()).hexdigest()
        require(cli_hash==self.evidence['cli']['sha256'],'Installed CLI differs from the verified package')
        cli_env={key:value for key,value in os.environ.items() if not key.startswith('OSS_TRANSFER_')}
        cli_env['OSS_TRANSFER_CLI_CONFIG_DIR']=str(self.root/'cli-profile')
        cli_env['OSS_TRANSFER_CONFIG_FILE']=str(self.root/'absent-v4-config')
        result=self.invoke([cli_path,'--host',self.origin,'--json','login','--username',self.credentials['username'],'--password-stdin'],env=cli_env,input=self.credentials['password']+'\n')
        require(result.returncode==0,'Installed CLI login failed')
        result=self.invoke([cli_path,'--host',self.origin,'--json','whoami'],env=cli_env)
        require(result.returncode==0 and self.credentials['username'] in result.stdout,'Installed CLI session is unusable')
        self.record('installed_cli_verified_and_can_log_in')
        config=self.root/'master/config/service.json'
        config_bytes=config.read_bytes()
        self.install('master',expect=1)
        require(self.pid('master')==before,'Repeat install disturbed existing service')
        require(config.read_bytes()==config_bytes,'Repeat install changed configuration')
        self.record('existing_install_requires_explicit_upgrade')
        invite=self.api('/api/master/agents/invitations',{'address':'lifecycle-agent','label':'persisted Agent label'})
        self.secrets.append(invite['inviteToken'])
        self.install('agent',token=invite['inviteToken'])
        agent_before=self.pid('agent')
        nodes=self.api('/api/master/nodes')['nodes']
        require(len(nodes)==1 and nodes[0]['label']=='persisted Agent label','Agent enrollment did not persist')
        member_id=nodes[0]['id']
        self.record('fresh_agent_service_authenticated',pid=agent_before,nodeId=member_id)
        agent_config=self.root/'agent/config/service.json'
        agent_bytes=agent_config.read_bytes()
        if self.args.all_installed:
            with self.latest_release() as release_url:
                self.install('master',upgrade=True,discover=True,discover_all=True,release_url=release_url)
        else:
            self.install('master',upgrade=True,discover=True)
        after=self.pid('master')
        require(after>0 and after!=before,'Master upgrade did not replace the service process')
        require(ready.with_name('ready.request').read_text()==ready.with_name('ready.committed').read_text(),
                'Successful upgrade retained an uncommitted startup')
        require(config.read_bytes()==config_bytes,'Upgrade modified configuration')
        self.login()
        nodes=self.api('/api/master/nodes')['nodes']
        require(len(nodes)==1 and nodes[0]['id']==member_id,'Master upgrade lost membership')
        self.record('discovered_master_upgrade_preserves_custom_paths_configuration_password_and_membership',before=before,after=after)
        if not self.args.all_installed: self.install('agent',upgrade=True,discover=True)
        require(self.pid('agent')!=agent_before,'Agent was not restarted')
        require(agent_config.read_bytes()==agent_bytes,'Agent configuration changed')
        nodes=self.api('/api/master/nodes')['nodes']
        require(len(nodes)==1 and nodes[0]['id']==member_id,'Agent upgrade re-enrolled or lost identity')
        self.record('discovered_agent_upgrade_preserves_custom_paths_and_identity_without_invitation')
        for role in ('master','agent'):
            installed=self.root/role/'bin'/('install.json' if self.system=='Windows' else 'install.tsv')
            if self.system=='Windows': metadata=json.loads(installed.read_text(encoding='utf-8-sig'))
            else: metadata=dict(line.split('\t',1) for line in installed.read_text(encoding='utf-8').splitlines())
            expected_cli=self.cli_path if role=='master' else self.root/role/'bin'/'oss-transfer-cli'
            if self.system=='Windows' and role=='agent': expected_cli=expected_cli.with_suffix('.exe')
            require(metadata['cliPath']==str(expected_cli), 'Discovered upgrade changed the custom CLI location')
            require(metadata['backupDir']==str(self.root/role/'backups'), 'Discovered upgrade changed the custom backup directory')
            require(any((self.root/role/'backups').iterdir()), 'Upgrade did not retain a backup in the custom directory')
            completed=list((self.root/role/'backups').glob('*/backup.json'))
            require(len(completed)==1, 'Successful upgrade did not mark exactly one complete rollback backup')
            marker=json.loads(completed[0].read_text())
            require(marker['state']=='COMPLETED' and marker['identity']['role']==role and
                    marker['identity']['serviceName']==self.names[role], 'Upgrade backup marker belongs to the wrong installation')
        if self.args.all_installed: self.record('unfiltered_upgrade_discovers_both_roles_and_preserves_configuration_password_membership_and_cli_paths')
        old_marker=next((self.root/'master/backups').glob('*/backup.json'))
        old_backup=old_marker.parent
        protected_backups=[path for path in (self.root/'master/backups').iterdir()
                           if path.is_dir() and not (path/'backup.json').exists()]
        marker=json.loads(old_marker.read_text())
        marker['completedAt']='2000-01-01T00:00:00Z'
        old_marker.write_text(json.dumps(marker)+'\n')
        legacy_before=self.pid('master')
        self.install('master',upgrade=True,release=self.legacy_release(),discover=True)
        require(self.pid('master')!=legacy_before and config.read_bytes()==config_bytes,'Legacy-format upgrade did not preserve the installed service')
        self.login()
        require(self.api('/api/master/nodes')['nodes'][0]['id']==member_id,'Legacy-format upgrade lost membership')
        self.record('legacy_manifest_without_archive_still_upgrades_existing_service')
        require(not old_backup.exists(), 'Expired completed backup was not removed after successful replacement')
        require(len(list((self.root/'master/backups').glob('*/backup.json')))==1, 'Retention removed the newest rollback backup')
        require(all(path.exists() for path in protected_backups), 'Retention removed an unrecognized or CLI-only backup')
        self.record('successful_upgrade_expires_old_backup_and_preserves_newest_and_unmanaged_backups')
        tampered=self.root/'tampered'
        shutil.copytree(self.release,tampered)
        archive=next(entry for entry in tampered.iterdir() if entry.name.endswith(('.tar.gz','.zip')))
        with archive.open('ab') as stream:stream.write(b'bad-archive-checksum')
        before=self.pid('master')
        self.install('master',upgrade=True,release=tampered,expect=1)
        require(self.pid('master')==before,'Checksum refusal stopped the Master')
        self.login()
        self.record('checksum_failure_refused_before_service_stop')
        cli_tampered=self.root/'cli-tampered'
        shutil.copytree(self.release,cli_tampered)
        corrupt_cli=self.root/'corrupt-cli-payload'
        corrupt_cli.write_bytes(self.payloads['cli'].read_bytes()+b'bad-cli-checksum')
        self.write_archive(cli_tampered,dict(self.payloads,cli=corrupt_cli),update_file_rows=False)
        self.install('master',upgrade=True,release=cli_tampered,expect=1)
        require(self.pid('master')==before and hashlib.sha256(cli_path.read_bytes()).hexdigest()==cli_hash,'Corrupt CLI disturbed installed service or CLI')
        self.record('cli_checksum_failure_refused_before_service_stop')
        failed=self.failure_release()
        backups_before=set((self.root/'master/backups').iterdir())
        self.install('master',upgrade=True,release=failed,expect=1,timeout=10)
        require(ready.with_name('ready.request').read_text()!=ready.with_name('ready.committed').read_text(),
                'Failed upgrade or rollback incorrectly committed remote scan cleanup')
        require(hashlib.sha256(cli_path.read_bytes()).hexdigest()==cli_hash,'Startup rollback did not restore the previous CLI')
        require(config.read_bytes()==config_bytes,'Rollback changed configuration')
        self.login()
        nodes=self.api('/api/master/nodes')['nodes']
        require(len(nodes)==1 and nodes[0]['id']==member_id,'Rollback lost committed membership')
        require((self.root/'master/data/master.db').stat().st_size>0,'Rollback lost Master database')
        failed_backups=set((self.root/'master/backups').iterdir())-backups_before
        require(len(failed_backups)==1 and all(path.is_dir() and not (path/'backup.json').exists()
                                             for path in failed_backups), 'Failed upgrade backup was marked for automatic cleanup')
        self.record('failed_start_rolls_back_binary_database_config_and_running_service',pid=self.pid('master'))
        # Simulate a stopped manual installation or finalized V4 import: retain
        # databases/configuration, but remove this test's service registrations
        # and managed binaries. Adoption must neither reset setup nor re-enroll.
        for role in ('agent','master'):
            self.remove_service(role)
            directory=self.root/role
            for part in ('bin','config'):
                shutil.move(directory/part,directory/('before-adoption-'+part))
        master_source=self.root/'master/before-adoption-config/service.json'
        self.install('master',expect=1)
        self.install('master',adopt_config=master_source,release=failed,expect=1,timeout=10,replace_cli=True)
        self.install('master',upgrade=True)
        self.login()
        require(config.read_bytes()==config_bytes,'Adoption replaced the existing configuration')
        if self.system=='Linux':
            require(self.invoke(['systemctl','is-enabled','--quiet',self.names['master']]).returncode==0,
                    'Successful retry did not enable the recreated service')
        self.record('failed_adoption_restores_database_and_can_retry_without_new_setup')
        self.install('agent',adopt_config=self.root/'agent/before-adoption-config/service.json')
        nodes=self.api('/api/master/nodes')['nodes']
        require(len(nodes)==1 and nodes[0]['id']==member_id,'Adoption re-enrolled the existing Agent')
        require(agent_config.read_bytes()==agent_bytes,'Adoption changed Agent configuration')
        self.record('adopt_existing_agent_without_invitation_preserves_identity')
        self.evidence['result']='passed'
        self.save()

    def remove_service(self,role):
        name=self.names[role]
        try: old_pid=self.pid(role)
        except (ValueError,subprocess.CalledProcessError,TypeError,OSError): old_pid=0
        if self.system=='Linux':
            self.invoke(['systemctl','stop',name])
            self.invoke(['systemctl','disable',name])
            (Path('/etc/systemd/system')/(name+'.service')).unlink(missing_ok=True)
            self.invoke(['systemctl','daemon-reload'])
            self.invoke(['systemctl','reset-failed',name])
        elif self.system=='FreeBSD':
            unit=Path('/usr/local/etc/rc.d')/name
            if unit.is_file():
                self.invoke([unit,'onestop'])
                unit.unlink()
        elif self.system=='Darwin':
            domain='system' if self.args.system else 'gui/'+str(os.getuid())
            self.invoke(['launchctl','bootout',domain+'/'+name])
            unit=Path('/Library/LaunchDaemons') if self.args.system else Path.home()/'Library/LaunchAgents'
            (unit/(name+'.plist')).unlink(missing_ok=True)
        else:
            self.invoke(['powershell.exe','-NoProfile','-Command',f"$s=Get-Service -Name '{name}' -ErrorAction SilentlyContinue; if($s){{Stop-Service -Name '{name}' -ErrorAction SilentlyContinue; $s.WaitForStatus('Stopped',[TimeSpan]::FromSeconds(50)); sc.exe delete '{name}'}}"])
        if self.system!='Windows' and old_pid:
            deadline=time.monotonic()+50
            while time.monotonic()<deadline:
                try:os.kill(old_pid,0)
                except ProcessLookupError:break
                time.sleep(.2)
            else:raise AssertionError('Owned service process did not exit during cleanup')

    def close(self):
        for role in reversed(self.roles):
            self.remove_service(role)
        for path in self.root.rglob('*.log'):
            path.write_text(self.scrub(path.read_text(errors='replace', encoding="utf-8")), encoding="utf-8")
        self.evidence['testServicesRemoved']=True
        self.save()


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--master',type=Path,required=True)
    parser.add_argument('--agent',type=Path,required=True)
    parser.add_argument('--cli',type=Path,required=True)
    parser.add_argument('--output',type=Path,required=True)
    parser.add_argument('--repo',type=Path,default=Path(__file__).resolve().parents[2])
    parser.add_argument('--target-platform',choices=[o+'/'+a for o,a in PREPARE.TARGETS.PLATFORMS],
                        help='Expected binary platform; foreign CPU execution requires Linux binfmt')
    parser.add_argument('--failure-master-fixture',type=Path,help='Precompiled target-platform startup failure fixture')
    parser.add_argument('--rollback-cli-fixture',type=Path,help='Precompiled target-platform replacement CLI fixture')
    parser.add_argument('--all-installed',action='store_true',help='Disposable CI hosts only: exercise unfiltered discovery of every installed role')
    parser.add_argument('--system',action='store_true',help='macOS: system LaunchDaemon (requires root)')
    args=parser.parse_args()
    if bool(args.failure_master_fixture) != bool(args.rollback_cli_fixture):
        parser.error('--failure-master-fixture and --rollback-cli-fixture must be supplied together')
    lifecycle=Lifecycle(args)
    try:lifecycle.run()
    finally:lifecycle.close()

if __name__=='__main__':main()
