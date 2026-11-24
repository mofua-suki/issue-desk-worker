// ビルドしたイメージの実際の版が定義どおりか。違えば発行前にビルドを止める。
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { execFileSync, spawnSync } from 'node:child_process';
import os from 'node:os';
import { createHash } from 'node:crypto';
import type { ImageVersions } from './image-versions.mts';

export const containerTools = Object.freeze(['node', 'sh', 'codex', 'claude', 'pnpm', 'agy', 'git', 'curl', 'caddy', 'java', 'gradle', 'flutter']);
const versionedTools = ['pnpm', 'codex', 'claude', 'agy', 'caddy', 'java', 'gradle', 'flutter'] as const;
const requiredCommands = ['sh', 'git', 'curl'] as const;
type VersionedTool = (typeof versionedTools)[number];
type RequiredCommand = (typeof requiredCommands)[number];

type ImageToolChecks = {
  versions: ImageVersions;
  node?: string;
  probe?: (cli: VersionedTool) => string;
  commandExists?: (cli: RequiredCommand) => string;
};

export function checkImageTools({
  versions,
  node = process.versions.node,
  probe = (cli) => execFileSync(cli, [cli === 'caddy' ? 'version' : '--version'], { encoding: 'utf8', timeout: ['java', 'gradle', 'flutter'].includes(cli) ? 120000 : 15000 }),
  commandExists = (cli) => execFileSync('sh', ['-c', 'command -v "$1"', 'sh', cli], { encoding: 'utf8', timeout: 15000 }).trim(),
}: ImageToolChecks): void {
  if (node !== versions.node) throw Error('Node の版がイメージの定義と違います');
  for (const cli of versionedTools) {
    const lines = probe(cli).split(/\r?\n/);
    const line =
      cli === 'codex'
        ? lines.find((s) => /^codex-cli\s/.test(s))
        : cli === 'claude'
          ? lines.find((s) => /Claude Code/.test(s))
          : cli === 'java'
            ? lines.find((s) => /^openjdk\s/.test(s))
            : cli === 'gradle'
              ? lines.find((s) => /^Gradle\s/.test(s))
              : cli === 'flutter'
                ? lines.find((s) => /^Flutter\s/.test(s))
                : lines[0];
    const current = cli === 'caddy' ? line?.match(/^v?(\d+\.\d+\.\d+)(?:\s|$)/)?.[1] : line?.match(/\b\d+\.\d+\.\d+\b/)?.[0];
    const key = cli === 'agy' ? 'antigravity' : cli === 'java' ? 'jdk' : cli;
    if (current !== versions[key]) throw Error(`${cli} の版がイメージの定義と違います`);
  }
  for (const cli of requiredCommands) {
    try {
      if (!commandExists(cli)) throw Error('command -v が空の結果を返しました');
    } catch (cause) {
      throw Error(`${cli} が受付のイメージにありません`, { cause });
    }
  }
}

export function checkChromium(): void {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'chromium-check-'));
  try {
    // コンテナ環境では非 root のユーザー名前空間を利用できない場合があるため、Chromium のサンドボックスを無効にする。
    const output = execFileSync('/usr/lib/chromium/chromium-headless-shell', ['--no-sandbox', '--headless', '--disable-gpu', `--user-data-dir=${dir}`, '--dump-dom', 'about:blank'], { encoding: 'utf8', timeout: 30000 });
    if (!output.includes('<html')) throw Error('Chromium が HTML を出力しませんでした');
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

function flutterState(root: string): string {
  const hash = createHash('sha256');
  const visit = (name: string): void => {
    const full = path.join(root, name);
    const stat = fs.lstatSync(full);
    hash.update(JSON.stringify([name, stat.mode, stat.size]));
    if (stat.isSymbolicLink()) hash.update(fs.readlinkSync(full));
    else if (stat.isDirectory()) for (const entry of fs.readdirSync(full).sort()) visit(path.join(name, entry));
    else if (stat.isFile()) {
      const fd = fs.openSync(full, 'r');
      try {
        const buffer = Buffer.allocUnsafe(1024 * 1024);
        let n;
        while ((n = fs.readSync(fd, buffer, 0, buffer.length, null)) !== 0) hash.update(buffer.subarray(0, n));
      } finally { fs.closeSync(fd); }
    } else throw Error(`Flutter SDK 内の不明なファイル: ${name}`);
  };
  for (const name of ['packages/flutter_tools', 'bin/cache/flutter_tools.snapshot', 'bin/cache/flutter_tools.stamp', 'bin/cache/engine-dart-sdk.stamp']) visit(name);
  for (const entry of fs.readdirSync(path.join(root, 'bin/cache')).sort()) {
    const name = path.join('bin/cache', entry);
    const stat = fs.lstatSync(path.join(root, name));
    if (stat.isDirectory() || stat.isSymbolicLink()) visit(name);
  }
  return hash.digest('hex');
}

function checkFlutterReadable(root: string): void {
  const visit = (full: string): void => {
    const stat = fs.lstatSync(full);
    if (stat.isSymbolicLink()) return;
    if (stat.isDirectory()) {
      if ((stat.mode & 0o005) !== 0o005) throw Error(`Flutter SDK のディレクトリを探索できません: ${full}`);
      try {
        for (const entry of fs.readdirSync(full)) visit(path.join(full, entry));
      } catch (error) {
        const { code, path: errorPath } = error as NodeJS.ErrnoException;
        if (code === 'EACCES' || code === 'EPERM') throw Error(`Flutter SDK を読めません: ${errorPath || full}`, { cause: error });
        throw error;
      }
    } else if (stat.isFile()) {
      if ((stat.mode & 0o004) === 0) throw Error(`Flutter SDK のファイルを読めません: ${full}`);
      try { fs.accessSync(full, fs.constants.R_OK); } catch (cause) { throw Error(`Flutter SDK のファイルを読めません: ${full}`, { cause }); }
    }
  };
  visit(root);
}

export function checkFlutterOutput(output: string): void {
  if (/Failed to update packages|Read-only file system|Building flutter tool|(?:write|writing|open|create|rename)[^\n]*flutter_tools\/\.dart_tool/i.test(output)) {
    throw Error('Flutter SDK の再生成または書き込み失敗を検出しました');
  }
}

export function checkFlutterPackageConfig(file: string): void {
  const config: { packages?: unknown } = JSON.parse(fs.readFileSync(file, 'utf8'));
  if (!Array.isArray(config.packages) || config.packages.length === 0) throw Error('Flutter package_config にパッケージがありません');
  for (const pkg of config.packages as { rootUri?: unknown }[]) {
    if (typeof pkg.rootUri !== 'string' || !pkg.rootUri) throw Error('Flutter package_config の rootUri がありません');
    const url = new URL(pkg.rootUri, pathToFileURL(file));
    if (url.protocol !== 'file:' || url.search || url.hash) throw Error(`不正な rootUri: ${pkg.rootUri}`);
    const target = fileURLToPath(url);
    const resolved = fs.realpathSync(target);
    for (const candidate of [target, resolved]) {
      if (candidate === '/home' || candidate.startsWith('/home/')) throw Error(`HOME 内の rootUri: ${pkg.rootUri}`);
    }
    const stat = fs.statSync(resolved);
    if (!stat.isDirectory()) throw Error(`rootUri がディレクトリではありません: ${pkg.rootUri}`);
    fs.accessSync(resolved, fs.constants.R_OK | fs.constants.X_OK);
  }
}

function reportFlutterFiles(root: string): void {
  for (const name of ['pubspec.yaml', 'pubspec.lock', '.dart_tool/package_config.json']) {
    const file = path.join(root, 'packages/flutter_tools', name);
    try {
      const stat = fs.statSync(file);
      let readable;
      try { fs.accessSync(file, fs.constants.R_OK); readable = 'yes'; }
      catch (error) { readable = `no (${(error as NodeJS.ErrnoException).code || 'unknown'})`; }
      console.error(`Flutter diagnostic ${file}: mode=${(stat.mode & 0o7777).toString(8)} uid=${stat.uid} gid=${stat.gid} mtime=${stat.mtime.toISOString()} readable=${readable}`);
    } catch (error) {
      console.error(`Flutter diagnostic ${file}: unavailable (${(error as NodeJS.ErrnoException).code || 'unknown'})`);
    }
  }
}

export function checkFlutterReadonly(root = '/opt/flutter'): void {
  try {
    checkFlutterReadonlyImpl(root);
  } catch (error) {
    try { reportFlutterFiles(root); } catch { /* Preserve the original failure. */ }
    throw error;
  }
}

function checkFlutterReadonlyImpl(root: string): void {
  const cache = path.join(root, 'bin/cache');
  const uid = process.getuid?.();
  const owner = fs.statSync(root).uid;
  checkFlutterReadable(root);
  const head = execFileSync('git', ['-C', root, 'rev-parse', 'HEAD'], { encoding: 'utf8', timeout: 15000 }).trim();
  const stamp = fs.readFileSync(path.join(cache, 'flutter_tools.stamp'), 'utf8').trim();
  console.log(`Flutter check uid=${uid} SDK owner uid=${owner}\nFlutter git HEAD=${head}\nflutter_tools.stamp=${stamp}`);
  if (stamp !== `${head}:${process.env.FLUTTER_TOOL_ARGS || ''}`) throw Error('Flutter SDK の stamp が不整合です');
  if (uid === undefined || uid === 0 || uid === owner) throw Error('Flutter SDK と異なる非root UID が必要です');
  checkFlutterPackageConfig(path.join(root, 'packages/flutter_tools/.dart_tool/package_config.json'));
  for (const p of ['/opt', root, path.join(root, 'packages/flutter_tools/.dart_tool'), path.join(cache, 'flutter_tools.snapshot')]) {
    try { fs.accessSync(p, fs.constants.W_OK); } catch { continue; }
    throw Error(`${p} は読み取り専用である必要があります`);
  }
  fs.accessSync(cache, fs.constants.W_OK);
  const before = flutterState(root);
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'flutter-readonly-'));
  try {
    fs.mkdirSync(path.join(dir, 'test'));
    fs.writeFileSync(path.join(dir, 'pubspec.yaml'), 'name: image_flutter_check\nenvironment:\n  sdk: ">=3.0.0 <4.0.0"\ndev_dependencies:\n  flutter_test:\n    sdk: flutter\n');
    fs.writeFileSync(path.join(dir, 'test/check_test.dart'), "import 'package:flutter_test/flutter_test.dart';\nvoid main() { test('image check', () { expect(1 + 1, 2); }); }\n");
    const env = { ...process.env, HOME: dir, PUB_CACHE: path.join(dir, 'pub-cache'), FLUTTER_SUPPRESS_ANALYTICS: 'true' };
    for (const args of [['--version'], ['test', '--reporter', 'expanded']]) {
      const result = spawnSync(path.join(root, 'bin/flutter'), args, { cwd: dir, env, encoding: 'utf8', timeout: 300000, stdio: ['ignore', 'pipe', 'pipe'], maxBuffer: 10 * 1024 * 1024 });
      const output = (result.stdout || '') + (result.stderr || '');
      console.log(output);
      checkFlutterOutput(output);
      if (result.error || result.status !== 0) throw Error(`flutter ${args[0]} failed: ${result.error || result.status}`);
      if (args[0] === 'test' && !/\+[1-9]\d*.*All tests passed!/.test(output)) throw Error('Flutter テストの合格結果がありません');
    }
    if (JSON.stringify(before) !== JSON.stringify(flutterState(root))) throw Error('Flutter SDK の保護対象が変更されました');
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  if (process.argv[2] === '--chromium') checkChromium();
  else if (process.argv[2] === '--flutter-readonly') checkFlutterReadonly(process.argv[3]);
  else checkImageTools({ versions: JSON.parse(fs.readFileSync(new URL('./versions.json', import.meta.url), 'utf8')) });
}
