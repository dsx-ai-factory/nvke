#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Pinned, disposable NVKE/Mokka replay. Created with AI assistance."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import sys

PACKAGE = Path(__file__).resolve().parent


def call(args, *, env=None, log=None, cwd=None):
    if log:
        with Path(log).open('a') as stream:
            subprocess.run(args, env=env, cwd=cwd, stdout=stream, stderr=subprocess.STDOUT, check=True)
        return ''
    return subprocess.run(args, env=env, cwd=cwd, text=True, capture_output=True, check=True).stdout.strip()


def save(path, data):
    path.write_text(json.dumps(data, indent=2) + '\n')


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def environment(work, config):
    env = os.environ.copy()
    # Previous developer-shell E2E switches are not inputs to this replay.
    for key in list(env):
        if key.startswith(('LOCAL_NKE_E2E_', 'LOCAL_NVKE_E2E_', 'CAPD_', 'NKE_E2E_',
                           'NODE_AGENT_', 'BUNDLE_', 'CNPG_', 'DGXC_TENANT_')):
            del env[key]
    state = work / 'private'
    state.mkdir(mode=0o700, exist_ok=True)
    for name in ('docker', 'bin', 'credentials'):
        (state / name).mkdir(mode=0o700, exist_ok=True)
    make = shutil.which(os.environ.get('REPLAY_MAKE', 'gmake')) or shutil.which('make')
    if not make or 'GNU Make' not in call([make, '--version']):
        raise RuntimeError('GNU Make is required; set REPLAY_MAKE to its executable')
    link = state / 'bin/make'
    if not link.exists():
        link.symlink_to(make)
    # Keep Docker registry reads anonymous without changing the user's Keychain settings.
    save(state / 'docker/config.json', {'auths': {'https://index.docker.io/v1/': {}}})
    buildx = shutil.which('docker-buildx')
    old_plugins = Path(env.get('DOCKER_CONFIG', str(Path.home() / '.docker'))) / 'cli-plugins/docker-buildx'
    if not buildx and old_plugins.is_file():
        buildx = str(old_plugins)
    if buildx:
        plugins = state / 'docker/cli-plugins'
        plugins.mkdir(exist_ok=True)
        link = plugins / 'docker-buildx'
        if not link.exists():
            link.symlink_to(Path(buildx).resolve())
    host = env.get('DOCKER_HOST') or call(['docker', 'context', 'inspect', '--format', '{{.Endpoints.docker.Host}}'])
    if not host.startswith('unix://'):
        raise RuntimeError('Use a local Unix-socket Docker daemon, not a remote Docker host')
    env.update(DOCKER_HOST=host, COLIMA_DOCKER_HOST=host,
               DOCKER_CONFIG=str(state / 'docker'), HELM_REGISTRY_CONFIG=str(state / 'docker/config.json'),
               XDG_CONFIG_HOME=str(state / 'config'),
               PATH=str(state / 'bin') + os.pathsep + str(work / 'source/cli/bin') + os.pathsep + env['PATH'],
               KIND_CLUSTER_NAME=config['kind'], BGP_CONTAINER_NAME=config['bgp'],
               BUNDLE_MIRROR_NAME=config['mirror'], BUNDLE_MIRROR_VOLUME=config['volume'],
               CAPD_NODE_IMAGE=config['image'], CAPD_KINDEST_NODE_IMAGE=config['image'],
               INSTALL_CNI_AFTER_INIT='false', CAPD_SPIRE_ATTESTATION_METHOD='joinToken',
               NODE_AGENT_SPIRE_ATTESTATION_METHOD='joinToken', LOCAL_NVKE_E2E_REUSE_DOCKER_HOST='true',
               LOCAL_NKE_E2E_STATE_DIR=str(state / 'focused'),
               LOCAL_NKE_E2E_KUBECONFIG=str(state / 'management.kubeconfig'),
               LOCAL_NKE_E2E_LABEL_FILTER='mokka', DGXC_TENANT_USER='mokka-replay')
    for key, name in [('DGXC_TENANT_PASSWORD', 'tenant'), ('CNPG_SUPERUSER_PASSWORD', 'postgres'),
                      ('CNPG_CLOUD_PASSWORD', 'cloud'), ('CNPG_TEMPORAL_PASSWORD', 'temporal')]:
        file = state / 'credentials' / name
        if not file.exists():
            file.write_text(secrets.token_hex(18))
            file.chmod(0o600)
        env[key] = file.read_text().strip()
    return env


def prepare(work, repository):
    if work.exists():
        raise RuntimeError('prepare needs a new directory; existing work is preserved')
    lock = json.loads((PACKAGE / 'versions.json').read_text())
    for filename, expected in lock['patches'].items():
        if digest(PACKAGE / filename) != expected:
            raise RuntimeError('Package patch checksum mismatch: ' + filename)
    work.mkdir(parents=True, mode=0o700)
    source = work / 'source'
    source.mkdir()
    log = work / 'prepare.log'
    for args in [['git', 'init', str(source)], ['git', '-C', str(source), 'remote', 'add', 'origin', repository],
                 ['git', '-C', str(source), 'fetch', '--depth=1', 'origin', lock['nvkeCommit']],
                 ['git', '-C', str(source), 'checkout', '-b', 'test/mokka-replay', 'FETCH_HEAD']]:
        call(args, log=log)
    if call(['git', '-C', str(source), 'rev-parse', 'HEAD']) != lock['nvkeCommit']:
        raise RuntimeError('Fetched revision differs from lock')
    for filename in lock['patches']:
        call(['git', '-C', str(source), 'apply', '--check', str(PACKAGE / filename)], log=log)
        call(['git', '-C', str(source), 'apply', str(PACKAGE / filename)], log=log)
    ident = secrets.token_hex(4)
    config = {'kind': 'nvke-replay-' + ident, 'bgp': 'nvke-replay-bgp-' + ident,
              'mirror': 'nvke-replay-bundles-' + ident, 'volume': 'nvke-replay-bundles-' + ident,
              'image': 'nvke-replay-node:' + ident, 'sourceCommit': lock['nvkeCommit'],
              'patchedFiles': {name: digest(source / name) for name in lock['patchedFiles']}}
    save(work / 'replay.json', config)
    print('Prepared pinned source and patches in', work)


def doctor(env):
    for name in ('git', 'python3', 'bash', 'docker', 'kind', 'kubectl', 'helm', 'go', 'jq',
                 'dpkg-deb', 'tilt', 'curl', 'openssl', 'sha256sum', 'tar', 'make'):
        if not shutil.which(name, path=env['PATH']):
            raise RuntimeError('Missing prerequisite: ' + name)
    if int(call(['bash', '-c', 'echo ${BASH_VERSINFO[0]}'], env=env)) < 4:
        raise RuntimeError('Bash 4+ is required on PATH')
    if 'go1.27.3 ' not in call(['go', 'version'], env=env):
        raise RuntimeError('This source pin requires Go 1.27.3; put that toolchain on PATH')
    info = json.loads(call(['docker', 'info', '--format', '{{json .}}'], env=env))
    if info['OSType'] != 'linux' or info['Architecture'] not in ('aarch64', 'arm64', 'x86_64', 'amd64'):
        raise RuntimeError('Need Linux arm64/amd64 Docker')
    if info['NCPU'] < 8 or info['MemTotal'] < 15 * 1024 ** 3:
        raise RuntimeError('Provide at least 8 CPUs and 16 GiB RAM to the Docker VM')
    host_containers = call(['docker', 'ps', '--filter', 'network=host', '--format', '{{.Names}} {{.Image}}'], env=env)
    if any('bgp' in line or 'frrouting' in line for line in host_containers.splitlines()):
        raise RuntimeError('A BGP container is running; run CAPD replay serially on a dedicated Docker host')
    print('Prerequisites checked; local Docker has', info['NCPU'], 'CPUs.')


def cleanup(work, config, env):
    # First use the normal API deletion path for any tenant recorded in this dedicated cluster.
    kubeconfig = work / 'private/management.kubeconfig'
    if kubeconfig.exists() and config['kind'] in call(['kind', 'get', 'clusters'], env=env).splitlines():
        kube = ['kubectl', '--kubeconfig', str(kubeconfig), '--context', 'kind-' + config['kind']]
        crds = json.loads(call(kube + ['get', 'crd', 'clusters.cluster.x-k8s.io',
                'machines.cluster.x-k8s.io', '--ignore-not-found', '-o', 'json'], env=env) or '{"items": []}')['items']
        resources = [c['metadata']['name'] for c in crds]
        def tenant_objects():
            return (json.loads(call(kube + ['get', ','.join(resources), '-A', '-o', 'json'], env=env))['items']
                    if resources else [])
        if tenant_objects():
            call([sys.executable, str(PACKAGE / 'mixed.py'), str(work), '--cleanup-only'], env=env)
        if tenant_objects():
            raise RuntimeError('Tenant CAPI resources remain; management cluster preserved for inspection')
        call(['kind', 'delete', 'cluster', '--name', config['kind']], env=env, log=work / 'private/cleanup.log')
    for name in (config['mirror'], config['bgp']):
        if call(['docker', 'ps', '-aq', '--filter', 'name=^/' + name + '$'], env=env):
            call(['docker', 'rm', '-f', name], env=env)
    if call(['docker', 'volume', 'ls', '-q', '--filter', 'name=^' + config['volume'] + '$'], env=env):
        call(['docker', 'volume', 'rm', config['volume']], env=env)
    save(work / 'cleanup.json', {'managementClusterRemoved': True, 'ownedMirrorAndBGPRemoved': True})
    print('Removed this replay management cluster, bundle mirror and BGP container. Public caches are retained.')


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['prepare', 'doctor', 'run', 'cleanup'])
    parser.add_argument('workdir', type=Path)
    parser.add_argument('--repository', default='git@github.com:dsx-ai-factory/nvke.git')
    parser.add_argument('--keep-tenant', action='store_true', help='retain a successful mixed tenant for follow-up work')
    args = parser.parse_args()
    work = args.workdir.expanduser().resolve()
    if args.command == 'prepare':
        prepare(work, args.repository)
        return
    config = json.loads((work / 'replay.json').read_text())
    if not re.fullmatch(r'nvke-replay-[0-9a-f]{8}', config['kind']):
        raise RuntimeError('Unrecognised management cluster name; refusing the target')
    ident = config['kind'].removeprefix('nvke-replay-')
    if any(config[key] != expected for key, expected in {
            'bgp': 'nvke-replay-bgp-' + ident, 'mirror': 'nvke-replay-bundles-' + ident,
            'volume': 'nvke-replay-bundles-' + ident, 'image': 'nvke-replay-node:' + ident}.items()):
        raise RuntimeError('Replay resource names disagree; refusing the target')
    env = environment(work, config)
    if args.command == 'cleanup':
        cleanup(work, config, env)
        return
    doctor(env)
    if args.command == 'doctor':
        return
    if (work / 'private/run-started').exists():
        raise RuntimeError('Use a new prepared directory for a repeat; existing results are preserved')
    (work / 'private/run-started').touch()
    for name, expected in config['patchedFiles'].items():
        if digest(work / 'source' / name) != expected:
            raise RuntimeError('Prepared source changed: ' + name)
    print('Building matching CLI; private logs:', work / 'private', flush=True)
    call(['make', '-C', str(work / 'source/cli'), 'build'], env=env, log=work / 'private/cli-build.log')
    print('Running focused Mokka/CAPD lifecycle checks...', flush=True)
    call(['make', '-C', str(work / 'source/controller'), 'test-e2e-capd-local'], env=env,
         log=work / 'private/focused.log')
    print('Focused lifecycle passed; running the mixed-model demo...', flush=True)
    command = [sys.executable, str(PACKAGE / 'mixed.py'), str(work)]
    if args.keep_tenant:
        command.append('--keep-tenant')
    call(command, env=env, log=work / 'private/mixed-helper.log')
    print('PASS: replay complete. Results:', work / 'results.json', flush=True)
    if args.keep_tenant:
        tenant = json.loads((work / 'private/tenant-record.json').read_text())['name']
        print('Tenant retained:', tenant, '; private kubeconfig:', work / 'private/tenant.kubeconfig', flush=True)


if __name__ == '__main__':
    try:
        main()
    except (RuntimeError, subprocess.CalledProcessError) as error:
        print('Replay stopped:', str(error) if isinstance(error, RuntimeError) else 'command failed; inspect private logs', file=sys.stderr)
        sys.exit(1)
