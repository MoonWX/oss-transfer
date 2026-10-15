#!/usr/bin/env python3
"""Small Java/Go comparison inside an existing resource-capped Linux cgroup.

Run only in a new, private QA directory. The caller creates the cgroup; this
script refuses uncapped execution. Disk monitoring is a soft guard, not a quota.
It never inspects installed services or uses their configuration or credentials.
"""
import argparse
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import time


GIB = 1024 ** 3


def allocated_bytes(root):
    # Python's per-file stat walk was measured at 2–3 seconds for 100k files.
    # GNU du counts directory blocks and hard-linked inodes in native code.
    # Retry once if a temporary file is published during traversal; an
    # incomplete size result must never silently disable the disk guard.
    for _ in range(2):
        result = subprocess.run(['/usr/bin/du', '-sx', '-B1', str(root)],
                                capture_output=True, text=True, timeout=30)
        if result.returncode == 0:
            return int(result.stdout.split()[0])
    raise RuntimeError('Cannot obtain complete allocated QA directory size')


def cgroup_directory():
    relative = next(line[3:] for line in Path('/proc/self/cgroup').read_text().splitlines() if line.startswith('0::'))
    return Path('/sys/fs/cgroup') / relative.lstrip('/')


def read_metrics(directory):
    result = {}
    for name in ('cpu.stat', 'memory.current', 'memory.peak', 'memory.events', 'io.stat', 'pids.current'):
        path = directory / name
        if path.exists():
            result[name] = path.read_text().strip()
    return result


def require_no_oom(initial, current):
    before = dict(line.split() for line in initial['memory.events'].splitlines())
    after = dict(line.split() for line in current['memory.events'].splitlines())
    if any(int(after[name]) > int(before[name]) for name in ('oom', 'oom_kill')):
        raise RuntimeError('QA cgroup reported an OOM event')


def remove_completed_runtime(output):
    # The child has exited and its process group has been stopped before this
    # call. Keep manifests, logs and samples; delete only this case's payload,
    # disposable databases and extracted binaries. No du traversal runs while
    # these directories disappear.
    for relative in ('fixtures', 'downloads', 'java-runtime', 'runtime/seaweed-data',
                     'runtime/master-data', 'runtime/agent-0-data', 'artifacts'):
        path = output / relative
        if path.is_symlink():
            raise RuntimeError('Unexpected symlink in QA cleanup: ' + relative)
        if path.is_dir():
            shutil.rmtree(path)


def stop(child):
    if child is None:
        return
    # Every comparison gets its own process group. Never match processes by
    # executable name, and never stop the system's installed transfer service.
    try:
        os.killpg(child.pid, signal.SIGTERM)
    except ProcessLookupError:
        return
    try:
        child.wait(timeout=20)
    except subprocess.TimeoutExpired:
        pass
    try:
        os.killpg(child.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('root', 'java', 'java-package', 'go-package', 'weed'):
        parser.add_argument('--' + name, type=Path, required=True)
    parser.add_argument('--component', nargs=4, metavar=('FIXTURE', 'DIRECTION', 'ENGINE', 'REPETITION'),
                        help='One long-workload component; externally aggregate alternating pairs')
    parser.add_argument('--poll-interval', type=float, default=1,
                        help='Identical task-status polling interval for both engines')
    args = parser.parse_args()
    if not 0.05 <= args.poll_interval <= 10:
        parser.error('poll-interval must be between 0.05 and 10 seconds')
    if args.component:
        fixture, direction, engine, repetition = args.component
        if fixture not in ('small', 'large') or direction not in ('LOCAL_TO_OSS', 'OSS_TO_OSS', 'OSS_TO_LOCAL') or engine not in ('java', 'go') or repetition not in ('1', '2', '3', '4', '5'):
            parser.error('Invalid component')
    root = args.root.resolve()
    if args.root.is_symlink() or not root.name.startswith('oss-cloud-qa-') or root.stat().st_mode & 0o077:
        raise RuntimeError('Expected a private oss-cloud-qa-* directory')
    results = root / ('results-' + '-'.join(args.component) if args.component else 'results')
    results.mkdir(mode=0o700, exist_ok=False)
    cgroup = cgroup_directory()
    limits = {name: (cgroup / name).read_text().strip() for name in ('cpu.max', 'memory.max', 'memory.swap.max', 'cpuset.cpus.effective')}
    quota, period = limits['cpu.max'].split()
    if quota == 'max' or int(quota) / int(period) > 2:
        raise RuntimeError('QA requires CPUQuota <= 200%')
    if limits['memory.max'] == 'max' or int(limits['memory.max']) > 3 * GIB or limits['memory.swap.max'] != '0':
        raise RuntimeError('QA requires MemoryMax <= 3 GiB and MemorySwapMax=0')
    if os.getpriority(os.PRIO_PROCESS, 0) < 10:
        raise RuntimeError('QA requires nice >= 10')
    evidence = {'status': 'RUNNING', 'cgroup': str(cgroup), 'limits': limits,
                'softAllocatedDiskGuardBytes': int(2.5 * GIB), 'guardIntervalSeconds': 2.0 if args.component else 0.5,
                'diskGuardIsQuota': False, 'existingProductionTaskStatus': 'UNKNOWN',
                'scope': 'One cloud host; private loopback SeaweedFS; no JD OSS or JPFS access',
                'component': args.component, 'fullPairedComparison': not bool(args.component),
                'cases': [], 'startedUnix': time.time(), 'initialMetrics': read_metrics(cgroup)}
    evidence_path = results / 'cloud-evidence.json'
    samples = (results / 'cgroup-samples.jsonl').open('w')
    child = None
    started = time.monotonic()

    def save():
        evidence_path.write_text(json.dumps(evidence, indent=2) + '\n')

    def interrupted(*_):
        raise RuntimeError('QA interrupted')

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    save()
    try:
        schedule = [args.component] if args.component else [
            (fixture, direction, None, None) for fixture in ('small', 'large', 'mixed')
            for direction in ('LOCAL_TO_OSS', 'OSS_TO_OSS', 'OSS_TO_LOCAL')]
        for fixture, direction, engine, repetition in schedule:
            output = results / (fixture + '-' + direction)
            command = [sys.executable, str(Path(__file__).with_name('compare.py')),
                       '--java', str(args.java), '--java-package', str(args.java_package),
                       '--go-package', str(args.go_package), '--weed', str(args.weed),
                       '--output', str(output), '--profile', 'cloud-small', '--fixtures', fixture,
                       '--directions', direction, '--repetitions', '3', '--node-bandwidth-mbps', '100000',
                       '--java-max-heap-mib', '768', '--go-memory-limit', '768MiB', '--seaweed-memory-limit', '512MiB',
                       '--keep-runtime']
            if args.component:
                command.extend(['--profile', 'cloud-long', '--paired-component', '--engines', engine,
                                '--repetitions', '1', '--repetition-start', repetition, '--task-timeout', '3000',
                                '--poll-interval', str(args.poll_interval)])
            record = {'fixture': fixture, 'direction': direction, 'command': command, 'metricsBefore': read_metrics(cgroup)}
            evidence['cases'].append(record)
            save()
            print('START ' + output.name, flush=True)
            with (results / (output.name + '.log')).open('w') as stream:
                child = subprocess.Popen(command, stdout=stream, stderr=subprocess.STDOUT, start_new_session=True)
                allocated, checked_at, scan_seconds = 0, 0.0, 0.0
                while child.poll() is None:
                    if time.time() - checked_at >= evidence['guardIntervalSeconds']:
                        scan_started = time.monotonic()
                        allocated = allocated_bytes(root)
                        scan_seconds = time.monotonic() - scan_started
                        checked_at = time.time()
                    row = {'unix': time.time(), 'elapsedSeconds': time.monotonic() - started,
                           'case': output.name, 'allocatedBytes': allocated, 'allocatedMeasuredAtUnix': checked_at,
                           'directoryScanSeconds': scan_seconds, 'cgroup': read_metrics(cgroup)}
                    samples.write(json.dumps(row) + '\n')
                    samples.flush()
                    require_no_oom(evidence['initialMetrics'], row['cgroup'])
                    if allocated > evidence['softAllocatedDiskGuardBytes']:
                        raise RuntimeError('QA directory exceeded the 2.5 GiB soft guard')
                    if row['elapsedSeconds'] > (57 if args.component else 27) * 60:
                        raise RuntimeError('QA reached its internal deadline')
                    time.sleep(0.5)
            record['exitCode'] = child.returncode
            stop(child)
            child = None
            record['metricsAfter'] = read_metrics(cgroup)
            require_no_oom(evidence['initialMetrics'], record['metricsAfter'])
            record['allocatedBytesBeforeCleanup'] = allocated_bytes(root)
            if record['allocatedBytesBeforeCleanup'] > evidence['softAllocatedDiskGuardBytes']:
                raise RuntimeError('QA directory exceeded the 2.5 GiB soft guard')
            save()
            if record['exitCode']:
                raise RuntimeError('Comparison failed; inspect ' + output.name + '.log')
            result = json.loads((output / 'results.json').read_text())
            if result['status'] != 'PASSED' or len(result['runs']) != (1 if args.component else 6):
                raise RuntimeError('Measured task count differs from the requested experiment')
            record['summary'] = result['summary']
            record['verifiedObjects'] = sum(run['sha256VerifiedObjects'] for run in result['runs'])
            remove_completed_runtime(output)
            record['allocatedBytesAfterCleanup'] = allocated_bytes(root)
            save()
            print('PASS ' + output.name, flush=True)
        require_no_oom(evidence['initialMetrics'], read_metrics(cgroup))
        evidence['status'] = 'PASSED'
    except BaseException as error:
        evidence['status'] = 'FAILED'
        evidence['error'] = str(error)
        raise
    finally:
        stop(child)
        evidence['finishedUnix'] = time.time()
        evidence['finalMetrics'] = read_metrics(cgroup)
        save()
        samples.close()


if __name__ == '__main__':
    main()
