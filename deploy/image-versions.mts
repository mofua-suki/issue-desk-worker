// Docker の準備用 Node から、安全なシェル変数を生成する（版の値を build arg や .env から受けない）。
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
const keys = ['node', 'pnpm', 'codex', 'claude', 'antigravity', 'caddy', 'jdk', 'gradle', 'flutter'] as const;
export type ImageVersions = Record<(typeof keys)[number], string>;

const versions: ImageVersions = JSON.parse(fs.readFileSync(new URL('./versions.json', import.meta.url), 'utf8'));
if (Object.keys(versions).length !== keys.length || keys.some((k) => !/^\d+\.\d+\.\d+$/.test(versions[k])))
  throw Error('deploy/versions.json の版を確認してください');
fs.writeFileSync(path.join(os.tmpdir(), 'image-versions.env'), keys.map((k) => `${k.toUpperCase()}_VERSION=${versions[k]}\n`).join(''));
