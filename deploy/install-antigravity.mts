// 最新版への追従をせず、公式の Linux x86_64/glibc の配布物を固定して入れる。
import fs from 'node:fs';
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';

import type { ImageVersions } from './image-versions.mts';

type AntigravityRelease = {
  version: string;
  sha512: string;
  publishedAt: string;
  url: string;
};

const release: AntigravityRelease = JSON.parse(fs.readFileSync(new URL('./antigravity-release.json', import.meta.url), 'utf8'));
const versions: ImageVersions = JSON.parse(fs.readFileSync(new URL('./versions.json', import.meta.url), 'utf8'));
if (process.arch !== 'x64') throw Error('Antigravity の固定配布物は Linux x86_64 用です');
if (release.version !== versions.antigravity || !/^[a-f0-9]{128}$/.test(release.sha512))
  throw Error('Antigravity の版・SHA512 を確認してください');
const published = Date.parse(release.publishedAt);
if (!Number.isFinite(published) || Date.now() - published < 14 * 86400000)
  throw Error('Antigravity の公開から14日経過していません');
const url = new URL(release.url);
if (url.origin !== 'https://storage.googleapis.com' || !url.pathname.startsWith('/antigravity-public/antigravity-cli/'))
  throw Error('Antigravity の公式配布元を確認してください');
const archive = '/tmp/antigravity.tar.gz';
execFileSync('curl', ['--fail', '--silent', '--show-error', '--location', '--proto', '=https', '-o', archive, url.href]);
try {
  const hash = createHash('sha512').update(fs.readFileSync(archive)).digest('hex');
  if (hash !== release.sha512) throw Error('Antigravity の SHA512 が一致しません');
  fs.mkdirSync('/opt/agent-tools/lib/antigravity', { recursive: true });
  execFileSync('tar', ['-xzf', archive, '-C', '/opt/agent-tools/lib/antigravity', 'antigravity']);
  fs.chmodSync('/opt/agent-tools/lib/antigravity/antigravity', 0o755);
} finally {
  fs.rmSync(archive, { force: true });
}
