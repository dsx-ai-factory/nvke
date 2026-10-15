#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
# Ubuntu's cloud-init image supplies Python. No script from the controller is
# executed in the agent cgroup after installation starts.
helper=$(mktemp /var/tmp/nvke-agent-upgrade.XXXXXX.py)
trap 'rm -f "$helper"' EXIT
cat > "$helper" <<'PY'
import datetime
import fcntl
import json
import os
from pathlib import Path
import platform
import re
import shlex
import shutil
import subprocess
import sys
import tarfile
import time
import urllib.parse
import urllib.request

ROOT = Path('/var/lib/nvke/agent-upgrades')
LEGACY_PACKAGE = 'nke-agent'
PACKAGE = 'nvke-agent'
# Keep the legacy state tree intact until the package migration moves it.
legacy_state = Path('/var/lib/nke')
if legacy_state.is_dir() and not legacy_state.is_symlink() and not ROOT.parent.exists():
    ROOT = legacy_state / 'agent-upgrades'


def select_layout(name=None):
    global SERVICE, BINARY, UNIT, BUNDLE_FILES
    if name is None:
        name = PACKAGE if Path('/usr/local/bin/' + PACKAGE).exists() else LEGACY_PACKAGE
    SERVICE = name + '.service'
    BINARY = Path('/usr/local/bin/' + name)
    UNIT = Path('/etc/systemd/system/' + SERVICE)
    BUNDLE_FILES = (BINARY, UNIT, Path('/usr/local/bin/' + name + '-updater'),
                    Path('/etc/systemd/system/' + name + '-updater@.service'))
    if name == PACKAGE:
        BUNDLE_FILES += (Path('/usr/local/lib/nvke-agent/migrate-agent-paths.sh'),)


select_layout()


def installed_package():
    for name in (PACKAGE, LEGACY_PACKAGE):
        try:
            fields = run('dpkg-query', '--show', '--showformat=${db:Status-Status} ${Version}', name).split()
            if len(fields) == 2 and fields[0] == 'installed':
                return name, fields[1]
        except subprocess.SubprocessError:
            pass
    raise RuntimeError('an installed node-agent package is required')


def available_package(version):
    for name in (PACKAGE, LEGACY_PACKAGE):
        try:
            if ('Package: ' + name) in run('apt-cache', 'show', name + '=' + version).splitlines():
                return name
        except subprocess.SubprocessError:
            pass
    raise RuntimeError('exact agent package is not available')


def run(*args, cwd=None):
    result = subprocess.run(args, cwd=cwd, check=True, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, text=True, timeout=1200)
    return result.stdout.strip()


def request_key(request):
    return request['targetVersion'] + '_' + request['method']


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat().replace('+00:00', 'Z')


def save(path, op):
    op['updatedAt'] = now()
    temp = path.with_suffix('.tmp')
    with temp.open('w') as handle:
        json.dump(op, handle)
        handle.flush()
        os.fsync(handle.fileno())
    os.chmod(temp, 0o600)
    os.replace(temp, path)


def verify(version):
    for _ in range(60):
        try:
            actual = run(str(BINARY), '--version').rsplit(':', 1)[-1].strip()
            run('systemctl', 'is-active', '--quiet', SERVICE)
            if actual == version:
                return
        except subprocess.SubprocessError:
            pass
        time.sleep(2)
    raise RuntimeError('agent did not become active at the exact target version')


def download(url, destination):
    if urllib.parse.urlparse(url).scheme not in ('https', 'http', 'file'):
        raise RuntimeError('bundle source must use HTTP, HTTPS, or file')
    with urllib.request.urlopen(url, timeout=120) as source, destination.open('wb') as output:
        shutil.copyfileobj(source, output)


def bundle_source(settings):
    server = settings.get('NKE_BUNDLE_SERVER_URL', '')
    index = settings.get('NKE_BUNDLE_INDEX_URL', '')
    arguments = shlex.split(settings.get('AGENT_ARGS', ''))
    for i, argument in enumerate(arguments):
        for flag, key in (('--bundle-server-url', 'server'), ('--index-file-url', 'index')):
            value = None
            if argument.startswith(flag + '='):
                value = argument.split('=', 1)[1]
            elif argument == flag and i + 1 < len(arguments):
                value = arguments[i + 1]
            if value is not None:
                if key == 'server':
                    server = value
                else:
                    index = value
    if not index:
        release = Path('/etc/os-release').read_text()
        ubuntu = '24' if '24.04' in release else '22' if '22.04' in release else ''
        if not ubuntu or not server:
            raise RuntimeError('agent bundle source is not configured for this OS')
        index = server.rstrip('/') + '/nke_bundles/ubuntu_' + ubuntu + '_index.json'
    return server, index


def bundle_url(version, source):
    server, index = source
    arch = platform.machine()
    with urllib.request.urlopen(index, timeout=120) as source:
        entries = json.load(source)['agent']['entries']
    aliases = ('x86_64', 'amd64') if arch == 'x86_64' else ('aarch64', 'arm64')
    for entry in entries:
        if entry['version'].lstrip('v') == version.lstrip('v') and entry['arch'] in aliases:
            return urllib.parse.urljoin(server.rstrip('/') + '/', entry['path'])
    raise RuntimeError('exact agent bundle is not available')


def install_bundle(directory):
    agents = [file for name in (PACKAGE, LEGACY_PACKAGE) for file in directory.rglob(name)]
    if len(agents) != 1:
        raise RuntimeError('bundle must contain one agent')
    name = agents[0].name
    units = list(directory.rglob(name + '.service'))
    if len(units) != 1:
        raise RuntimeError('bundle must contain one agent service unit')
    run('systemctl', 'stop', SERVICE)
    if SERVICE != name + '.service':
        run('systemctl', 'disable', SERVICE)
        UNIT.unlink(missing_ok=True)
    if name == PACKAGE:
        migrations = list(directory.rglob('migrate-agent-paths.sh'))
        if len(migrations) != 1:
            raise RuntimeError('renamed agent bundle must contain one migration script')
        run('bash', str(migrations[0]))
        destination = Path('/usr/local/lib/nvke-agent/migrate-agent-paths.sh')
        destination.parent.mkdir(parents=True, exist_ok=True)
        run('install', '-m', '0755', str(migrations[0]), str(destination))
    select_layout(name)
    BINARY.unlink(missing_ok=True)
    UNIT.unlink(missing_ok=True)
    run('install', '-m', '0755', str(agents[0]), str(BINARY))
    run('install', '-m', '0644', str(units[0]), str(UNIT))
    # Preserve the current independent updater when the target predates it.
    for suffix, destination, mode in (
            ('-updater', '/usr/local/bin/' + name + '-updater', '0755'),
            ('-updater@.service', '/etc/systemd/system/' + name + '-updater@.service', '0644')):
        matches = list(directory.rglob(name + suffix))
        if len(matches) > 1:
            raise RuntimeError('bundle contains duplicate updater files')
        if len(matches) == 1:
            Path(destination).unlink(missing_ok=True)
            run('install', '-m', mode, str(matches[0]), destination)
    if name == PACKAGE:
        alias = BINARY.with_name(LEGACY_PACKAGE)
        alias.unlink(missing_ok=True)
        alias.symlink_to(BINARY)
    run('systemctl', 'daemon-reload')
    run('systemctl', 'enable', '--force', SERVICE)
    run('systemctl', 'restart', SERVICE)


def execute(path):
    op = json.loads(path.read_text())
    assets = path.parent / 'assets'
    assets.mkdir(mode=0o700, exist_ok=True)
    op['phase'] = 'Running'
    op['step'] = 'staging'
    save(path, op)
    mutated = False
    previous_package = ''
    previous_name = ''
    previous_layout = BINARY.name
    previous_files = BUNDLE_FILES
    try:
        if op['method'] == 'apt':
            run('apt-get', 'update')
            previous_name, previous_package = installed_package()
            target_name = available_package(op['targetVersion'][1:])
            op['previousPackageName'] = previous_name
            op['previousPackageVersion'] = previous_package
            save(path, op)
            for package in set((previous_name + '=' + previous_package,
                                target_name + '=' + op['targetVersion'][1:])):
                run('apt-get', 'download', package, cwd=assets)
            mutated = True
            run('apt-get', '-y', '--allow-downgrades', '--reinstall', 'install',
                target_name + '=' + op['targetVersion'][1:])
            select_layout(target_name)
            if installed_package() != (target_name, op['targetVersion'][1:]):
                raise RuntimeError('installed agent package does not match the target')
            run('systemctl', 'restart', SERVICE)
        else:
            for file in BUNDLE_FILES:
                if file.exists():
                    shutil.copy2(file, assets / file.name)
                else:
                    (assets / (file.name + '.absent')).touch()
            archive = assets / 'target.tgz'
            op['artifactUrl'] = bundle_url(op['targetVersion'], op['bundleSource'])
            save(path, op)
            download(op['artifactUrl'], archive)
            extracted = assets / 'target'
            extracted.mkdir(exist_ok=True)
            with tarfile.open(archive) as bundle:
                # Reject escaping names, links and devices before extraction.
                for member in bundle.getmembers():
                    if member.name.startswith('/') or '..' in Path(member.name).parts or not (member.isfile() or member.isdir()):
                        raise RuntimeError('unsafe agent bundle member')
                bundle.extractall(extracted)
            mutated = True
            install_bundle(extracted)
        verify(op['targetVersion'])
        op.update(phase='Succeeded', step='verified', errorMessage='')
    except Exception as error:
        op.update(errorMessage=str(error), step='failed')
        try:
            if mutated:
                if op['method'] == 'apt':
                    packages = [p for p in assets.glob('*.deb')
                                if run('dpkg-deb', '--field', str(p), 'Version') == previous_package
                                and run('dpkg-deb', '--field', str(p), 'Package') == previous_name]
                    if len(packages) != 1:
                        raise RuntimeError('retained previous agent package is missing')
                    run('apt-get', '-y', '--allow-downgrades', 'install', str(packages[0]))
                    select_layout(previous_name)
                    run('systemctl', 'restart', SERVICE)
                else:
                    run('systemctl', 'stop', SERVICE)
                    if BINARY.name != previous_layout:
                        run('systemctl', 'disable', SERVICE)
                        UNIT.unlink(missing_ok=True)
                    for file in previous_files:
                        if (assets / (file.name + '.absent')).exists():
                            file.unlink(missing_ok=True)
                        else:
                            file.unlink(missing_ok=True)
                            mode = '0644' if file.name.endswith('.service') else '0755'
                            run('install', '-m', mode, str(assets / file.name), str(file))
                    select_layout(previous_layout)
                    run('systemctl', 'daemon-reload')
                    run('systemctl', 'enable', '--force', SERVICE)
                    run('systemctl', 'restart', SERVICE)
            verify(op['previousVersion'])
            op['phase'] = 'FailedRestored'
        except Exception as restoration:
            op['phase'] = 'FailedNotRestored'
            op['errorMessage'] += '; restoration: ' + str(restoration)
    save(path, op)


def read_operations(root):
    records = []
    for path in root.glob('*/operation.json'):
        try:
            op = json.loads(path.read_text())
            key = request_key(op)
            if path.parent.name != key or not re.fullmatch(r'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)_(apt|bundle)', key):
                continue
            if not all(isinstance(op[field], str) for field in ('phase', 'createdAt')):
                continue
        except (OSError, ValueError, KeyError, TypeError):
            continue
        records.append((path.parent, op))
    return records


def prune_operations(records):
    operations = [op for _, op in records]
    # Retain active requests and recent success and failure.
    keep = {request_key(op) for op in operations if op['phase'] in ('Pending', 'Running')}
    for candidates in ([op for op in operations if op['phase'] == 'Succeeded'],
                       [op for op in operations if op['phase'] in ('FailedRestored', 'FailedNotRestored')]):
        if candidates:
            keep.add(request_key(max(candidates, key=lambda op: op['createdAt'])))
    for folder, previous in records:
        name = request_key(previous)
        if name not in keep:
            if folder.name != name or not re.fullmatch(r'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)_(apt|bundle)', name):
                continue
            shutil.rmtree(folder)


if len(sys.argv) == 3 and sys.argv[1] == '--execute':
    execute(Path(sys.argv[2]))
    sys.exit(0)

target, method, observed = sys.argv[1:]
if target and not re.fullmatch(r'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)', target) or method not in ('apt', 'bundle'):
    raise RuntimeError('invalid exact agent target or installation method')
ROOT.mkdir(mode=0o700, parents=True, exist_ok=True)
with (ROOT / 'upgrade.lock').open('w') as lock:
    fcntl.flock(lock, fcntl.LOCK_EX)
    records = read_operations(ROOT)
    operations = [op for _, op in records]
    active = [op for op in operations if op['phase'] in ('Pending', 'Running')]
    current = active[0] if active else max(operations, key=lambda op: op['createdAt'], default=None)
    if active:
        unit = ('nvke-agent-upgrade-' + request_key(current)) if (ROOT / request_key(current) / 'updater.py').exists() else ('nvke-agent-updater@' + request_key(current) + '.service')
        state = subprocess.run(['systemctl', 'show', '--property=ActiveState', '--value', unit],
                               capture_output=True, text=True, timeout=15).stdout.strip()
        if state in ('active', 'activating', 'deactivating'):
            print(json.dumps(current))
            sys.exit(0)
        if not (ROOT / request_key(current) / 'updater.py').exists():
            run('systemctl', 'start', '--no-block', unit)
            print(json.dumps(current))
            sys.exit(0)
        # A reboot or killed updater can leave durable state ahead of systemd.
        current.update(phase='FailedNotRestored', errorMessage='independent updater stopped before completion')
        save(ROOT / request_key(current) / 'operation.json', current)
        print(json.dumps(current))
        sys.exit(0)
    if not target or observed == target:
        print(json.dumps({'phase': 'Succeeded', 'errorMessage': ''}))
        sys.exit(0)
    settings = {}
    # Read settings without evaluating shell or exposing service credentials.
    env_file = Path('/etc/default/' + BINARY.name)
    if env_file.exists():
        for line in env_file.read_text().splitlines():
            if '=' in line and not line.lstrip().startswith('#'):
                key, value = line.split('=', 1)
                values = shlex.split(value)
                settings[key.strip()] = values[0] if len(values) == 1 else value
    pid = run('systemctl', 'show', '--property=MainPID', '--value', SERVICE)
    if pid.isdigit() and pid != '0':
        environment = Path('/proc/' + pid + '/environ').read_bytes().split(b'\0')
        for item in environment:
            if b'=' in item:
                key, value = item.decode().split('=', 1)
                if key in ('NKE_BUNDLE_SERVER_URL', 'NKE_BUNDLE_INDEX_URL'):
                    settings[key] = value
        arguments = Path('/proc/' + pid + '/cmdline').read_bytes().split(b'\0')
        settings['AGENT_ARGS'] = shlex.join([item.decode() for item in arguments[1:] if item])
    prune_operations(records)
    op = dict(targetVersion=target,
              previousVersion=observed, method=method, phase='Pending', step='queued',
              errorMessage='', createdAt=now(), updatedAt=now())
    if method == 'bundle':
        op['bundleSource'] = bundle_source(settings)
    directory = ROOT / request_key(op)
    if directory.exists():
        shutil.rmtree(directory)
    directory.mkdir(mode=0o700)
    path = directory / 'operation.json'
    save(path, op)
    # The Python body is available through stdin to this bootstrap process.
    # Copy it from the enclosing script, then launch an independent unit.
    helper = directory / 'updater.py'
    # The shell records the exact body before executing this bootstrap.
    helper.write_text(Path(os.environ['NVKE_UPGRADE_HELPER']).read_text())
    os.chmod(helper, 0o700)
    try:
        run('systemd-run', '--quiet', '--no-block', '--collect', '--unit=nvke-agent-upgrade-' + request_key(op),
            '--property=Type=oneshot', '--property=TimeoutStartSec=30min',
            '/usr/bin/python3', str(helper), '--execute', str(path))
    except Exception as error:
        op.update(phase='FailedRestored', errorMessage=str(error))
        save(path, op)
    result = dict(op)
    if current and current.get('targetVersion') == target and current.get('method') == method and current.get('errorMessage') and current['phase'] in ('FailedRestored', 'FailedNotRestored'):
        result['errorMessage'] = current['errorMessage']
    print(json.dumps(result))
PY
NVKE_UPGRADE_HELPER="$helper" python3 "$helper" "$@"
