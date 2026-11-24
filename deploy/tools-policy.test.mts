import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { verifyRelease, verifyArchive } from './install-agent-tools.mts';
import { checkImageTools, checkFlutterPackageConfig } from './check-image-tools.mts';
import type { ImageVersions } from './image-versions.mts';
import type { SdkReleases } from './install-sdks.mts';
import type { SpawnSyncReturns } from 'node:child_process';
import { execFileSync, spawnSync, spawn } from 'node:child_process';
import http from 'node:http';
import net from 'node:net';
import { setTimeout as delay } from 'node:timers/promises';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
const now = Date.parse('2026-11-01T00:00:00Z');
const data = Buffer.from('fixed-package');
const integrity = 'sha512-' + createHash('sha512').update(data).digest('base64');
test('FlutterReadonlyRejectsInconsistentSDK', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'flutter-inconsistent-'));
  try {
    const sdk = path.join(dir, 'sdk');
    const bin = path.join(dir, 'bin');
    fs.mkdirSync(path.join(sdk, 'bin/cache'), { recursive: true });
    fs.mkdirSync(bin);
    fs.writeFileSync(path.join(bin, 'git'), '#!/bin/sh\nprintf "0123456789abcdef0123456789abcdef01234567\\n"\n', { mode: 0o755 });
    fs.writeFileSync(path.join(sdk, 'bin/cache/flutter_tools.stamp'), 'incorrect:\n');
    const result = spawnSync(process.execPath, [new URL('./check-image-tools.mts', import.meta.url).pathname, '--flutter-readonly', sdk], {
      encoding: 'utf8', env: { ...process.env, PATH: bin, FLUTTER_TOOL_ARGS: '' },
    });
    assert.equal(result.status, 1);
    assert.match(result.stderr, /stamp が不整合/);
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});
test('FlutterReadonlyDetectsRebuildOutput', () => {
  assert.notEqual(process.getuid?.(), 0, 'この権限検査は非rootで実行してください');
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'flutter-output-'));
  try {
    const sdk = path.join(dir, 'sdk');
    const cache = path.join(sdk, 'bin/cache');
    const tools = path.join(sdk, 'packages/flutter_tools/.dart_tool');
    const bin = path.join(dir, 'bin');
    fs.mkdirSync(cache, { recursive: true });
    fs.mkdirSync(tools, { recursive: true });
    fs.mkdirSync(bin);
    fs.writeFileSync(path.join(bin, 'git'), '#!/bin/sh\nprintf "revision\\n"\n', { mode: 0o755 });
    fs.writeFileSync(path.join(sdk, 'bin/flutter'), '#!/bin/sh\nif [ "$FLUTTER_FIXTURE_OUTPUT" = EXIT ]; then exit 42; elif [ "$1" = test ]; then printf "00:00 +1: All tests passed!\\n"; else printf "%s\\n" "$FLUTTER_FIXTURE_OUTPUT"; fi\n', { mode: 0o755 });
    for (const [name, value] of [['flutter_tools.stamp', 'revision:'], ['engine-dart-sdk.stamp', 'engine'], ['flutter_tools.snapshot', 'snapshot']]) fs.writeFileSync(path.join(cache, name), value, { mode: 0o444 });
    fs.writeFileSync(path.join(tools, 'package_config.json'), JSON.stringify({ packages: [{ rootUri: '../' }] }), { mode: 0o444 });
    for (const name of ['pubspec.yaml', 'pubspec.lock']) fs.writeFileSync(path.join(tools, '..', name), 'fixture', { mode: 0o444 });
    fs.chmodSync(tools, 0o555);
    fs.chmodSync(sdk, 0o555);
    // The fixture simulates a different UID; image builds verify actual ownership and permissions.
    const preload = path.join(dir, 'uid.cjs');
    fs.writeFileSync(preload, 'process.getuid = () => 987654;\n');
    const run = (output: string) => spawnSync(process.execPath, ['--require', preload, new URL('./check-image-tools.mts', import.meta.url).pathname, '--flutter-readonly', sdk], {
      encoding: 'utf8', env: { ...process.env, PATH: bin, FLUTTER_TOOL_ARGS: '', FLUTTER_FIXTURE_OUTPUT: output },
    });
    const checkDiagnostics = (result: SpawnSyncReturns<string>, missing = '') => {
      for (const name of ['pubspec.yaml', 'pubspec.lock', '.dart_tool/package_config.json']) {
        const file = path.join(sdk, 'packages/flutter_tools', name);
        if (name === missing) {
          assert.ok(result.stderr.includes(`Flutter diagnostic ${file}: unavailable (ENOENT)`));
          continue;
        }
        const stat = fs.statSync(file);
        let readable;
        try { fs.accessSync(file, fs.constants.R_OK); readable = 'yes'; }
        catch (error) { readable = `no (${(error as NodeJS.ErrnoException).code || 'unknown'})`; }
        assert.ok(result.stderr.includes(`Flutter diagnostic ${file}: mode=${(stat.mode & 0o7777).toString(8)} uid=${stat.uid} gid=${stat.gid} mtime=${stat.mtime.toISOString()} readable=${readable}`));
      }
    };
    assert.equal(run('Flutter 3.47.9').status, 0);
    fs.chmodSync(sdk, 0o755);
    const unreadable = path.join(sdk, 'unreadable');
    fs.writeFileSync(unreadable, 'private', { mode: 0o000 });
    let result = run('Flutter 3.47.9');
    assert.equal(result.status, 1);
    assert.ok(result.stderr.includes(unreadable));
    fs.chmodSync(unreadable, 0o444);
    const inaccessible = path.join(sdk, 'inaccessible');
    fs.mkdirSync(inaccessible, { mode: 0o700 });
    result = run('Flutter 3.47.9');
    assert.equal(result.status, 1);
    assert.ok(result.stderr.includes(inaccessible));
    fs.chmodSync(inaccessible, 0o755);
    fs.chmodSync(sdk, 0o555);
    for (const output of ['Building flutter tool...', 'Failed to update packages.', 'Read-only file system', 'Cannot open file /opt/flutter/packages/flutter_tools/.dart_tool/package_config.json']) {
      const rejected = run(output);
      assert.equal(rejected.status, 1, output);
      checkDiagnostics(rejected);
    }
    result = run('EXIT');
    assert.equal(result.status, 1);
    assert.match(result.stderr, /flutter --version failed: 42/);
    checkDiagnostics(result);
    const config = path.join(tools, 'package_config.json');
    fs.chmodSync(config, 0o000);
    result = run('Flutter 3.47.9');
    assert.equal(result.status, 1);
    assert.ok(result.stderr.includes('Flutter SDK のファイルを読めません'));
    checkDiagnostics(result);
    assert.match(result.stderr, /package_config\.json:.*readable=no \(EACCES\)/);
    fs.chmodSync(tools, 0o755);
    fs.unlinkSync(config);
    fs.chmodSync(tools, 0o555);
    fs.chmodSync(path.join(cache, 'flutter_tools.stamp'), 0o644);
    fs.writeFileSync(path.join(cache, 'flutter_tools.stamp'), 'incorrect:');
    result = run('Flutter 3.47.9');
    assert.equal(result.status, 1);
    assert.match(result.stderr, /stamp が不整合/);
    checkDiagnostics(result, '.dart_tool/package_config.json');
  } finally {
    for (const name of ['sdk', 'sdk/packages/flutter_tools/.dart_tool']) {
      try { fs.chmodSync(path.join(dir, name), 0o755); } catch (error) { if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error; }
    }
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
test('Flutter package roots must be accessible and outside home', () => {
  assert.notEqual(process.getuid?.(), 0, 'この権限検査は非rootで実行してください');
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'flutter-roots-'));
  try {
    const config = path.join(dir, 'package_config.json');
    const good = path.join(dir, 'package');
    fs.mkdirSync(good);
    const check = (rootUri: string) => {
      fs.writeFileSync(config, JSON.stringify({ packages: [{ rootUri }] }));
      checkFlutterPackageConfig(config);
    };
    assert.doesNotThrow(() => check('package/'));
    assert.throws(() => check('missing/'));
    fs.chmodSync(good, 0o000);
    assert.throws(() => check('package/'));
    fs.chmodSync(good, 0o755);
    assert.throws(() => check('file:///home/'));
    fs.symlinkSync('/home', path.join(dir, 'home-link'));
    assert.throws(() => check('home-link/'));
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});
const manifest = (published = '2026-10-01T00:00:00Z', origin = 'https://registry.npmjs.org') => ({time:{'1.2.3':published}, versions:{'1.2.3':{name:'@example/cli',version:'1.2.3',dist:{tarball:origin+'/@example/cli/-/cli-1.2.3.tgz',integrity}}}});
test('固定版・公開14日・SHA512を照合する', () => {const release=verifyRelease(manifest(),'@example/cli','1.2.3',now);assert.equal(release.integrity,integrity);assert.doesNotThrow(()=>verifyArchive(data,release.integrity));assert.throws(()=>verifyArchive(Buffer.from('tampered'),release.integrity));});
test('公開直後・欠測・別版・非公式配布元は導入しない', () => {assert.throws(()=>verifyRelease(manifest('2026-10-30T00:00:00Z'),'@example/cli','1.2.3',now));assert.throws(()=>verifyRelease(manifest('unknown'),'@example/cli','1.2.3',now));assert.throws(()=>verifyRelease(manifest(),'@example/cli','9.9.9',now));assert.throws(()=>verifyRelease(manifest(undefined,'https://other.example'),'@example/cli','1.2.3',now));});
test('Codex 固定3配布物だけ公開直後に許可する', () => {
  const pins = {
    '0.169.1': 'sha512-2ZP53HuNspqL1lCX9Xc2XfoSq26VjjQ7XC08dTnkpetzH0kkAN1TTM3K1YOSwzAqikCdjQUzMV0haZmdxhPhjg==',
    '0.169.1-linux-x64': 'sha512-KmlHQ/lB9jcn8Yy5z/tWCpSPsqBvI3pU+0PUPccr+pKiiU2NJSCvZcAD7s3dmIFq5ZvGYtGcSn2KdFFdaPQICg==',
    '0.169.1-linux-arm64': 'sha512-6vRE7ZxkngT6L5JQ9WUQ2UnztS6hqyfcQiFqvk0yhcvWsrZLJNiiDZcKzGbLfJK2sDy7tSt9uWdP1cIgMHIkaw==',
  };
  const release = (name: string, version: string, sha: string, published = '2026-11-01T00:00:00Z', origin = 'https://registry.npmjs.org') => {
    const time: Record<string, string | undefined> = { [version]: published };
    return {
      time,
      versions: { [version]: { name, version, dist: { tarball: `${origin}/${name}/-/codex-${version}.tgz`, integrity: sha } } },
    };
  };
  const name = '@openai/codex';
  for (const [version, sha] of Object.entries(pins)) {
    const metadata = release(name, version, sha);
    assert.equal(verifyRelease(metadata, name, version, now).integrity, sha);
    assert.throws(() => verifyRelease(release(name, version, integrity), name, version, now));
    for (const published of [undefined, 'invalid', '2026-11-02T00:00:00Z']) {
      const bad = release(name, version, sha);
      bad.time[version] = published;
      assert.throws(() => verifyRelease(bad, name, version, now));
    }
    assert.throws(() => verifyRelease(release(name, version, sha, '2026-11-01T00:00:00Z', 'https://example.com'), name, version, now));
    const badVersion = release(name, version, sha);
    badVersion.versions[version].version = '0.169.2';
    assert.throws(() => verifyRelease(badVersion, name, version, now));
  }
  for (const [otherName, version] of [[name, '0.169.2'], ['pnpm', '0.169.1']]) {
    assert.throws(() => verifyRelease(release(otherName, version, pins['0.169.1']), otherName, version, now));
    assert.doesNotThrow(() => verifyRelease(release(otherName, version, pins['0.169.1'], '2026-10-18T00:00:00Z'), otherName, version, now));
  }
});
test('イメージの版定義は9種類を必須とする', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'image-versions-'));
  try {
    fs.copyFileSync(new URL('./image-versions.mts', import.meta.url), path.join(dir, 'image-versions.mts'));
    const versions: ImageVersions = JSON.parse(fs.readFileSync(new URL('./versions.json', import.meta.url), 'utf8'));
    const run = (value: Partial<ImageVersions> & { extra?: string }) => {
      fs.writeFileSync(path.join(dir, 'versions.json'), JSON.stringify(value));
      execFileSync(process.execPath, [path.join(dir, 'image-versions.mts')], { stdio: 'pipe', env: { ...process.env, TMPDIR: dir } });
    };
    assert.doesNotThrow(() => run(versions));
    const { caddy, ...missing } = versions;
    assert.throws(() => run(missing));
    assert.throws(() => run({ ...versions, extra: caddy }));
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});
test('Caddy の実際の版が定義と違えばイメージを拒否する', () => {
  const versions: ImageVersions = JSON.parse(fs.readFileSync(new URL('./versions.json', import.meta.url), 'utf8'));
  const outputs = { pnpm: versions.pnpm, codex: `codex-cli ${versions.codex}`, claude: `${versions.claude} (Claude Code)`, agy: versions.antigravity, caddy: `v${versions.caddy} h1:hash`, java: `openjdk ${versions.jdk} 2025-10-21 LTS\nOpenJDK Runtime Environment`, gradle: `\n------------------------------------------------------------\nGradle ${versions.gradle}\n------------------------------------------------------------`, flutter: `Flutter ${versions.flutter} • channel stable • https://github.com/flutter/flutter.git\nTools • Dart 3.11.0` };
  const options = { versions, node: versions.node, probe: (cli: keyof typeof outputs) => outputs[cli], commandExists: () => '/bin/tool' };
  assert.doesNotThrow(() => checkImageTools(options));
  outputs.caddy = 'v9.9.9 h1:hash';
  assert.throws(() => checkImageTools(options));
});
test('JDK・Gradle・Flutter の実際の版が定義と違えばイメージを拒否する', () => {
  const versions: ImageVersions = JSON.parse(fs.readFileSync(new URL('./versions.json', import.meta.url), 'utf8'));
  const good = { pnpm: versions.pnpm, codex: `codex-cli ${versions.codex}`, claude: `${versions.claude} (Claude Code)`, agy: versions.antigravity, caddy: `v${versions.caddy}`, java: `openjdk ${versions.jdk} 2025-10-21 LTS`, gradle: `Welcome\nGradle ${versions.gradle}`, flutter: `Flutter ${versions.flutter} • channel stable` };
  for (const [cli, bad] of [['java', 'openjdk 21.0.9 2025-10-21 LTS'], ['gradle', 'Gradle 8.14.3'], ['flutter', 'Flutter 3.35.0 • channel stable'], ['flutter', '']] as const) {
    const outputs = { ...good, [cli]: bad };
    assert.throws(() => checkImageTools({ versions, node: versions.node, probe: (c) => outputs[c], commandExists: () => '/bin/tool' }), new RegExp(`^Error: ${cli} の版`));
  }
});
test('SDK の配布物は公式の配布元・定義の版・SHA256 だけを受ける', async () => {
  const { checkSdkRelease } = await import('./install-sdks.mts');
  const versions: ImageVersions = JSON.parse(fs.readFileSync(new URL('./versions.json', import.meta.url), 'utf8'));
  const releases: SdkReleases = JSON.parse(fs.readFileSync(new URL('./sdk-release.json', import.meta.url), 'utf8'));
  for (const name of ['jdk', 'gradle', 'flutter'] as const) assert.doesNotThrow(() => checkSdkRelease(name, releases[name], versions[name]));
  assert.throws(() => checkSdkRelease('gradle', { ...releases.gradle, url: 'https://example.com/distributions/gradle-9.8.2-bin.zip' }, versions.gradle));
  assert.throws(() => checkSdkRelease('gradle', releases.gradle, '9.8.20'));
  assert.throws(() => checkSdkRelease('flutter', releases.flutter, '3.47.1'));
  assert.throws(() => checkSdkRelease('jdk', { ...releases.jdk, sha256: 'abc' }, versions.jdk));
});
test('構築された Caddy は定義した版と標準モジュールを提供する', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'caddy-build-'));
  try {
    const versions: ImageVersions = JSON.parse(fs.readFileSync(new URL('./versions.json', import.meta.url), 'utf8'));
    let caddy = process.env.ISSUE_DESK_IMAGE_CADDY;
    if (caddy === undefined) {
      const env = { ...process.env, TMPDIR: dir };
      execFileSync(process.execPath, [new URL('./image-versions.mts', import.meta.url).pathname], { env });
      execFileSync('sh', [new URL('./install-caddy.sh', import.meta.url).pathname, path.join(dir, 'image-versions.env'), dir], { env, stdio: 'pipe' });
      caddy = path.join(dir, 'caddy');
    }
    const version = execFileSync(caddy, ['version'], { encoding: 'utf8' });
    assert.match(version, new RegExp(`^v${versions.caddy.replaceAll('.', '\\.')}([\\s]|$)`));
    const modules = execFileSync(caddy, ['list-modules'], { encoding: 'utf8' });
    assert.ok(modules.split(/\r?\n/).includes('http.handlers.reverse_proxy'));
    await checkCaddyHTTP(caddy);
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});

async function checkCaddyHTTP(caddy: string): Promise<void> {
  const upstream = http.createServer((_req, res) => { res.end('caddy-proxy-check'); });
  await new Promise<void>((resolve, reject) => upstream.once('error', reject).listen(0, '127.0.0.1', resolve));
  const port = (upstream.address() as net.AddressInfo).port;
  const reservation = net.createServer();
  await new Promise<void>((resolve, reject) => reservation.once('error', reject).listen(0, '127.0.0.1', resolve));
  const proxyPort = (reservation.address() as net.AddressInfo).port;
  await new Promise<void>((resolve, reject) => reservation.close((err) => err ? reject(err) : resolve()));
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'caddy-http-'));
  const config = path.join(dir, 'Caddyfile');
  fs.writeFileSync(config, `{\n admin off\n auto_https off\n}\nhttp://127.0.0.1:${proxyPort} {\n reverse_proxy 127.0.0.1:${port}\n}\n`);
  const child = spawn(caddy, ['run', '--config', config, '--adapter', 'caddyfile'], { stdio: ['ignore', 'ignore', 'pipe'] });
  const closed = new Promise<void>((resolve) => child.once('close', () => resolve()));
  let stderr = '';
  child.stderr.on('data', (chunk) => { stderr += chunk.toString(); });
  let failure: Error | undefined;
  child.on('error', (err) => { failure = err; });
  child.on('exit', (code, signal) => { failure = Error(`Caddy exited: ${code ?? signal} ${stderr}`); });
  try {
    const deadline = Date.now() + 15000;
    for (;;) {
      if (failure) throw failure;
      try {
        const response = await fetch(`http://127.0.0.1:${proxyPort}/`, { signal: AbortSignal.timeout(1000) });
        assert.equal(response.status, 200);
        assert.equal(await response.text(), 'caddy-proxy-check');
        break;
      } catch (err) {
        if (err instanceof assert.AssertionError) throw err;
        if (Date.now() >= deadline) throw Error(`Caddy HTTP startup failed: ${stderr}`, { cause: err });
        await delay(100);
      }
    }
  } finally {
    if (child.exitCode === null && child.signalCode === null) {
      child.kill('SIGKILL');
    }
    await closed;
    await new Promise<void>((resolve, reject) => upstream.close((err) => err ? reject(err) : resolve()));
    fs.rmSync(dir, { recursive: true, force: true });
  }
}
