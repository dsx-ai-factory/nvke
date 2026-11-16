#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Create a disposable tenant using the pinned NVKE fixtures, then run Ben's demo.

Created with AI assistance. No imports from earlier experiment directories.
"""
import argparse
import json
from pathlib import Path
import re
import shlex
import socket
import subprocess
import time
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET


def run(args):
    return subprocess.run(args, text=True, capture_output=True, check=True).stdout


def wait(label, probe, seconds=1200):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        result = probe()
        if result:
            print(label + ': ready', flush=True)
            return result
        time.sleep(5)
    raise TimeoutError(label)


class Client:
    def __init__(self, work):
        self.work = work
        self.private = work / 'private'
        self.config = json.loads((work / 'replay.json').read_text())
        self.mgmt = ['kubectl', '--kubeconfig', str(self.private / 'management.kubeconfig'),
                     '--context', 'kind-' + self.config['kind']]

    def __enter__(self):
        config = json.loads(run(self.mgmt + ['config', 'view', '--minify', '-o', 'json']))
        if urllib.parse.urlparse(config['clusters'][0]['cluster']['server']).hostname != '127.0.0.1':
            raise RuntimeError('Refusing non-loopback management API')
        self.token = run(self.mgmt + ['create', 'token', 'nke-client', '-n', 'nke-clients',
                                     '--audience=https://nke.tilt.local', '--duration=1h']).strip()
        services = json.loads(run(self.mgmt + ['get', 'svc', '-A', '-o', 'json']))['items']
        service = next(s for s in services if s['metadata']['name'] == 'nvke-controller')
        with socket.socket() as s:
            s.bind(('127.0.0.1', 0))
            port = s.getsockname()[1]
        self.url = 'http://127.0.0.1:' + str(port)
        self.forward = subprocess.Popen(self.mgmt + ['-n', service['metadata']['namespace'], 'port-forward',
                                        'svc/nvke-controller', str(port) + ':8080'],
                                        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            for _ in range(30):
                try:
                    self.api('clusters')
                    return self
                except OSError:
                    if self.forward.poll() is not None:
                        break
                    time.sleep(1)
            raise RuntimeError('Local controller port-forward did not become ready')
        except BaseException:
            self.__exit__()
            raise

    def __exit__(self, *_):
        self.forward.terminate()
        self.forward.wait(timeout=10)

    def api(self, path, method='GET', data=None):
        request = urllib.request.Request(self.url + '/api/' + path, method=method,
                                         data=None if data is None else json.dumps(data).encode(),
                                         headers={'Authorization': 'Bearer ' + self.token, 'Content-Type': 'application/json'})
        with urllib.request.urlopen(request, timeout=30) as response:
            body = response.read()
            return json.loads(body) if body else None

    def nodes(self, cluster):
        return self.api('nodes?clusterID=' + cluster['id'])['nodes']

    def delete(self, cluster):
        if not cluster['name'].startswith(('mokka-replay-', 'e2e-capd-')):
            raise RuntimeError('Unrecognised tenant name; refusing cleanup')
        names = [n['name'] for n in self.nodes(cluster)]
        self.api('clusters/' + cluster['id'], 'DELETE')
        wait('Tenant deleted through NVKE', lambda: not any(c['id'] == cluster['id']
             for c in self.api('clusters')['clusters']), seconds=900)
        containers = set(run(['docker', 'ps', '-a', '--format', '{{.Names}}']).splitlines())
        if containers.intersection(names):
            raise RuntimeError('Tenant node containers remain after deletion')
        resources = json.loads(run(self.mgmt + ['get', 'clusters,machines', '-A', '-o', 'json']))['items']
        if resources:
            raise RuntimeError('Tenant CAPI resources remain after deletion')


def create_request(client, repo):
    fixtures = repo / 'controller/test/e2e/internal/nvke'
    mirror = json.loads(run(['docker', 'inspect', client.config['mirror']]))[0]
    address = mirror['NetworkSettings']['Networks']['kind']['IPAddress']
    source = (fixtures / 'request.go').read_text()
    raw = re.search(r'`(sh -ec .*?)`,\n\s*shellQuote\(bundleServerURL\)', source, re.S)[1]
    command = raw.replace('%%', '__PERCENT__').replace('%s', shlex.quote('http://' + address + '/')).replace('__PERCENT__', '%')
    request = json.loads((fixtures / 'capd_cluster.json').read_text())
    request['name'] = 'mokka-replay-' + time.strftime('%Y%m%d-%H%M%S', time.gmtime())
    request['infra']['cluster']['metadata']['name'] = request['name']
    # This single-control-plane smoke does not need kube-vip/BGP. The focused
    # suite separately exercises the VIP path; avoiding it here prevents a
    # cold kube-vip start from racing the agent's immediate CNI installation.
    request.pop('controlPlaneVIP', None)
    request['controlPlaneKubernetesVersion'] = re.search(r'defaultKubernetesVersion\s*=\s*"([^"]+)"',
            (repo / 'controller/test/e2e/tenant_test.go').read_text())[1]
    cp = json.loads((fixtures / 'capd_control_plane_config.json').read_text())
    cp.update(desiredCount=1, systemReservedMemory='1Gi')
    worker = json.loads((fixtures / 'capd_nodepool.json').read_text())
    worker.update(name='worker', nodeNamePrefix='w', desiredNodeCount=1)
    for pool in (cp, worker):
        pool['infra']['machineTemplate']['spec']['template']['spec']['customImage'] = client.config['image']
        pool['infra']['preKubeadmCommands'] = [command, 'mkdir -p /run/cluster-api',
                                             'echo success > /run/cluster-api/bootstrap-success.complete']
    request.update(controlPlaneConfig=cp, nodePools=[worker])
    return request


def smoke(client, cluster, repo):
    nodes = client.nodes(cluster)
    cp = next(n for n in nodes if n['controlPlane'])['name']
    kubeconfig = client.private / 'tenant.kubeconfig'
    kubeconfig.write_text(run(['docker', 'exec', cp, 'cat', '/etc/kubernetes/admin.conf']))
    kubeconfig.chmod(0o600)
    cluster_config = json.loads(run(['kubectl', '--kubeconfig', str(kubeconfig), 'config', 'view', '-o', 'json']))['clusters'][0]
    server_name = urllib.parse.urlparse(cluster_config['cluster']['server']).hostname
    port = json.loads(run(['docker', 'inspect', cp]))[0]['NetworkSettings']['Ports']['6443/tcp'][0]['HostPort']
    run(['kubectl', '--kubeconfig', str(kubeconfig), 'config', 'set-cluster', cluster_config['name'],
         '--server=https://127.0.0.1:' + port, '--tls-server-name=' + server_name])
    run(['nvkectl', 'config', 'set-site', '--name', 'replay', '--url', client.url,
         '--kubeconfig', str(client.private / 'management.kubeconfig')])
    import os
    env = dict(os.environ, CLUSTER=cluster['name'], TENANT_KUBECONFIG=str(kubeconfig), PROFILES='gb300 h100')
    with (client.private / 'mixed.log').open('w') as log:
        subprocess.run(['bash', str(repo / 'hack/mokka-demo/mixed-gpus/run.sh')], env=env, cwd=repo,
                       stdout=log, stderr=subprocess.STDOUT, check=True, timeout=3600)
    kube = ['kubectl', '--kubeconfig', str(kubeconfig)]
    nodes = json.loads(run(kube + ['get', 'nodes', '-o', 'json']))['items']
    pods = json.loads(run(kube + ['get', 'pods', '-n', 'gpu-operator', '-o', 'json']))['items']
    checks = [line for line in (client.private / 'mixed.log').read_text().splitlines() if line.startswith('PASS  ')]
    cases = [c for c in ET.parse(client.private / 'focused/junit.xml').iter('testcase')
             if c.get('name', '').startswith('[It]') and c.find('skipped') is None]
    if len(cases) != 9 or any(c.find('failure') is not None or c.find('error') is not None for c in cases):
        raise RuntimeError('Expected nine successful focused lifecycle cases')
    workers = [{'node': n['metadata']['name'], 'product': n['metadata']['labels'].get('nvidia.com/gpu.product'),
                'gpu': int(n['status']['allocatable'].get('nvidia.com/gpu', '0')),
                'kubelet': n['status']['nodeInfo']['kubeletVersion']}
               for n in nodes if n['status']['allocatable'].get('nvidia.com/gpu')]
    if {(w['product'], w['gpu']) for w in workers} != {('NVIDIA-GB300-NVL', 4), ('NVIDIA-H100-80GB-HBM3', 8)}:
        raise RuntimeError('Unexpected model/capacity result')
    validators = [{'node': p['spec']['nodeName'], 'stages': {c['name']: c.get('state', {}).get('terminated', {}).get('exitCode')
                   for c in p['status'].get('initContainerStatuses', [])}}
                  for p in pods if p['metadata']['labels'].get('app') == 'nvidia-operator-validator']
    if len(checks) != 19 or len(validators) != 2 or any(len(v['stages']) != 4 or any(c != 0 for c in v['stages'].values()) for v in validators):
        raise RuntimeError('Missing mixed-model checks or successful validators')
    return {'sourceCommit': client.config['sourceCommit'], 'focusedPassed': len(cases), 'mixedPassed': len(checks),
            'workers': workers, 'validators': validators, 'passedChecks': checks}


def main():
    import os
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('workdir', type=Path)
    parser.add_argument('--cleanup-only', action='store_true')
    parser.add_argument('--keep-tenant', action='store_true')
    args = parser.parse_args()
    work = args.workdir.resolve()
    with Client(work) as client:
        existing = client.api('clusters')['clusters']
        if args.cleanup_only:
            for cluster in existing:
                client.delete(cluster)
            return
        if existing:
            raise RuntimeError('Dedicated management plane already contains a tenant')
        cluster = None
        success = False
        try:
            request = create_request(client, work / 'source')
            (client.private / 'tenant-request.json').write_text(json.dumps(request, indent=2) + '\n')
            cluster = client.api('clusters', 'POST', request)
            (client.private / 'tenant-record.json').write_text(json.dumps({'id': cluster['id'], 'name': request['name']}) + '\n')
            wait('Initial tenant provisioned', lambda: len(client.nodes(cluster)) == 2 and all(
                 n['conditions'].get('provisioned', {}).get('status') == 'True' for n in client.nodes(cluster)))
            print('Running mixed-model smoke...', flush=True)
            report = smoke(client, cluster, work / 'source')
            success = True
        finally:
            if cluster and not (success and args.keep_tenant):
                client.delete(cluster)
        report['tenantDeleted'] = not args.keep_tenant
        (work / 'results.json').write_text(json.dumps(report, indent=2) + '\n')
        print('PASS: 9 focused lifecycle cases and 19 mixed-model checks.', flush=True)
        if args.keep_tenant:
            print('Tenant retained:', cluster['name'], '; kubeconfig:', client.private / 'tenant.kubeconfig')
        else:
            print('Tenant cleanup passed; use replay.py cleanup to remove the dedicated management plane.')


if __name__ == '__main__':
    main()
