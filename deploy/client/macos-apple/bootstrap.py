#!/usr/bin/env python3
"""Initialize a dedicated Docker Desktop worker; never adopt foreign volumes."""
import argparse
import json
import os
import pathlib
import re
import subprocess
import time
import urllib.parse
import urllib.request

def run(args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)

def secret(path):
    p = pathlib.Path(path).resolve(strict=True)
    st = p.stat()
    if not p.is_file() or st.st_uid != os.getuid() or st.st_mode & 0o077:
        raise SystemExit('秘密ファイルは本人所有・他ユーザーアクセス不可で用意してください: ' + str(p))
    return p

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None

def bootstrap(a):
    linux = json.loads(pathlib.Path(a.config).read_text())
    if set(linux) != {'serverUrl', 'serverTokenFile', 'workerId'} or not all(isinstance(v, str) for v in linux.values()):
        raise SystemExit('K2はserverUrl/serverTokenFile/workerIdの3項目が必要です')
    host = linux['workerId']
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,63}', host):
        raise SystemExit('workerIdの形式が不正です')
    if linux['serverTokenFile'] != '/state/private/pc-token':
        raise SystemExit('Linux K2のserverTokenFileは/state/private/pc-tokenにしてください')
    files = {name: secret(getattr(a, name)) for name in
             ['pc_token_file', 'gitea_token_file', 'host_image_token_file', 'codex_auth_file', 'claude_auth_file']}
    url = urllib.parse.urlsplit(linux['serverUrl'])
    if url.scheme != 'https' or not url.netloc or url.username or url.password or url.query or url.fragment or url.path not in ['', '/']:
        raise SystemExit('受付のHTTPS URLが必要です')
    endpoint = linux['serverUrl'].rstrip('/') + '/api/v1/hosts/' + urllib.parse.quote(host, safe='') + '/desired-image?' + urllib.parse.urlencode({'flavor': a.flavor, 'currentRevision': ''})
    token = files['host_image_token_file'].read_text().strip()
    if not re.fullmatch(r'idh_[A-Za-z0-9_-]{43}', token):
        raise SystemExit('host image token fileが不正です')
    req = urllib.request.Request(endpoint, headers={'Authorization': 'Bearer ' + token, 'Accept': 'application/json'})
    try:
        with urllib.request.build_opener(NoRedirect).open(req, timeout=30) as response:
            distribution = json.load(response)
    except Exception:
        raise SystemExit('配布情報を取得できません。host image token・ホスト登録・HTTPS接続を確認してください')
    if not isinstance(distribution, dict) or 'desired' not in distribution:
        raise SystemExit('配布応答にdesiredがありません。受付の配布設定を確認してください')
    desired = distribution['desired']
    if desired is None:
        raise SystemExit('指定flavorに一致する配布がありません。ホスト登録と配布flavorを確認してください')
    if not isinstance(desired, dict) or not all(isinstance(desired.get(key), str) for key in
                                               ['revision', 'registry', 'digest', 'tag', 'flavor']):
        raise SystemExit('配布情報の形式または必須項目が不正です。受付の配布設定を確認してください')
    revision = desired.get('revision', '')
    registry = desired.get('registry', '')
    digest = desired.get('digest', '')
    tag = desired.get('tag', '')
    suffix = '-client-python' if a.flavor == '-python' else ''
    if not (re.fullmatch('[0-9a-f]{40}', revision) and tag == revision[:12] + suffix
            and desired.get('flavor') == a.flavor and re.fullmatch('sha256:[0-9a-f]{64}', digest)
            and re.fullmatch(r'[a-z0-9][a-z0-9.-]*(?::[0-9]{1,5})?', registry)):
        raise SystemExit('配布情報のtag/registry/revision/digest/flavorが不正です')
    # Select and verify a local Docker Desktop daemon explicitly; never use
    # DOCKER_HOST or a remote context for implementation or validation.
    inspect = ['docker', 'context', 'inspect', 'desktop-linux', '--format', '{{.Endpoints.docker.Host}}']
    found = subprocess.run(inspect, capture_output=True, text=True)
    if found.returncode:
        run(['open', '-a', 'Docker'])
        for _ in range(60):
            found = subprocess.run(inspect, capture_output=True, text=True)
            if not found.returncode:
                break
            time.sleep(2)
        else:
            raise SystemExit('Docker Desktopの初回起動・利用規約承認とdesktop-linux contextを確認してください')
    endpoint = found.stdout.strip()
    context = run(['docker', 'context', 'show'], capture_output=True, text=True).stdout.strip()
    if context != 'desktop-linux' or not endpoint.startswith('unix:///'):
        raise SystemExit('ローカルDocker Desktopのdesktop-linux contextが必要です。現在の設定は変更しません')
    docker = ['docker', '--context', 'desktop-linux']
    if subprocess.run(docker + ['info'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode:
        run(['open', '-a', 'Docker'])
        for _ in range(60):
            if subprocess.run(docker + ['info'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
                break
            time.sleep(2)
        else:
            raise SystemExit('Docker Desktopが起動しません。初回利用規約と画面の診断を確認してください')
    image = registry + '/issue-desk-client' + a.flavor + '@' + digest
    project = 'issue-desk-' + host.lower().replace('.', '-')
    volumes = {kind: project + '-' + kind for kind in ['state', 'codex', 'cli', 'agy']}
    root = pathlib.Path(a.root)
    manifest = root / 'docker-bootstrap.json'
    identity = {'host': host, 'volumes': volumes, 'config': linux}
    if manifest.exists() and json.loads(manifest.read_text()) != identity:
        raise SystemExit('既存のDocker導入記録とK2が一致しません。上書きしません')
    # Inspect everything before creating anything. Only our own labelled volumes
    # from a previous interrupted setup may be resumed.
    for name in volumes.values():
        found = subprocess.run(docker + ['volume', 'inspect', name], capture_output=True, text=True)
        if found.returncode == 0:
            labels = json.loads(found.stdout)[0].get('Labels') or {}
            if not manifest.exists() or labels.get('issue-desk.bootstrap') != host:
                raise SystemExit('別Workerまたは所有不明のvolumeを上書きしません: ' + name)
    containers = run(docker + ['ps', '-aq', '--filter', 'label=com.docker.compose.project=' + project],
                     capture_output=True, text=True).stdout.strip()
    if containers:
        raise SystemExit('既存Workerのコンテナがあります。引取り停止と実行・送信完了を確認してください: ' + project)
    if not manifest.exists():
        with manifest.open('x') as f:
            json.dump(identity, f)
    for name in volumes.values():
        if subprocess.run(docker + ['volume', 'inspect', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode:
            run(docker + ['volume', 'create', '--label', 'issue-desk.bootstrap=' + host, name], stdout=subprocess.DEVNULL)
        found = run(docker + ['volume', 'inspect', name], capture_output=True, text=True)
        if (json.loads(found.stdout)[0].get('Labels') or {}).get('issue-desk.bootstrap') != host:
            raise SystemExit('volumeの所有ラベルが一致しません: ' + name)
    run(docker + ['pull', '--platform', 'linux/amd64', image])
    # Copy through stdin, with exclusive creation, as the image's normal user.
    # Docker named volumes inherit the image's existing ownership; no chmod/chown.
    def place(volume, destination, data):
        mount = '/state' if volume == volumes['state'] else '/data/codex' if volume == volumes['codex'] else '/data/claude'
        script = """const fs=require('fs'),path=require('path');
const p=process.argv[1],data=fs.readFileSync(0);
fs.mkdirSync(path.dirname(p),{recursive:true});
if(fs.existsSync(p)){if(!fs.readFileSync(p).equals(data)){console.error('既存ファイルが入力と異なります。上書きしません');process.exit(1)}}
else fs.writeFileSync(p,data,{flag:'wx',mode:0o600});
"""
        run(docker + ['run', '--rm', '-i', '--platform', 'linux/amd64',
             '--mount', 'type=volume,source=' + volume + ',target=' + mount,
             '--entrypoint', 'node', image, '-e', script, mount + '/' + destination], input=data)
    place(volumes['state'], 'config.local.json', (json.dumps(linux) + '\n').encode())
    place(volumes['state'], 'private/pc-token', files['pc_token_file'].read_bytes())
    place(volumes['state'], 'private/worker-token', files['gitea_token_file'].read_bytes())
    place(volumes['codex'], 'auth.json', files['codex_auth_file'].read_bytes())
    place(volumes['cli'], '.credentials.json', files['claude_auth_file'].read_bytes())
    overlay = {'name': project, 'services': {'issue-desk-client': {
        'platform': 'linux/amd64', 'image': image}},
        'volumes': {'issue-desk-worker-' + kind: {'name': name, 'external': True} for kind, name in volumes.items()}}
    pathlib.Path(a.overlay).write_text(json.dumps(overlay))
    env = dict(os.environ, REGISTRY=registry, FLAVOR=a.flavor, ISSUE_DESK_CLIENT_TAG=tag,
               ISSUE_DESK_CLIENT_BLUE_TAG=tag, CODEX_AUTH_DIR='issue-desk-worker-codex',
               CLAUDE_API_KEY_FILE='/dev/null')
    cmd = docker + ['compose', '--env-file', '/dev/null', '-p', project, '-f', a.compose, '-f', a.overlay]
    run(cmd + ['run', '--rm', '--no-deps', '--entrypoint', '/usr/local/bin/worker',
               'issue-desk-client', 'check', '--config', '/state/config.local.json'], env=env)
    run(cmd + ['up', '-d', 'issue-desk-client'], env=env)

if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    for name in ['config', 'compose', 'root', 'overlay', 'pc-token-file', 'gitea-token-file',
                 'host-image-token-file', 'codex-auth-file', 'claude-auth-file']:
        parser.add_argument('--' + name, required=True)
    parser.add_argument('--flavor', choices=['', '-python'], default='-python')
    args = parser.parse_args()
    try:
        bootstrap(args)
    except subprocess.CalledProcessError as error:
        raise SystemExit('Dockerセットアップに失敗しました: ' + str(error.cmd[1]) + ' (終了コード ' + str(error.returncode) + ')')
