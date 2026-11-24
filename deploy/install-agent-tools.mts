// 固定したCLIだけを公開時刻・公式tarball・SHA512に照合して導入する。
import fs from 'node:fs';
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import type { ImageVersions } from './image-versions.mts';

type ReleaseEntry = {
  name?: unknown;
  version?: unknown;
  dist?: { tarball?: string; integrity?: string };
};

type RegistryMetadata = {
  versions?: Record<string, ReleaseEntry | undefined>;
  time?: Record<string, string | undefined>;
};

const codexIntegrity: Record<string, string> = {
  '0.169.1': 'sha512-2ZP53HuNspqL1lCX9Xc2XfoSq26VjjQ7XC08dTnkpetzH0kkAN1TTM3K1YOSwzAqikCdjQUzMV0haZmdxhPhjg==',
  '0.169.1-linux-x64': 'sha512-KmlHQ/lB9jcn8Yy5z/tWCpSPsqBvI3pU+0PUPccr+pKiiU2NJSCvZcAD7s3dmIFq5ZvGYtGcSn2KdFFdaPQICg==',
  '0.169.1-linux-arm64': 'sha512-6vRE7ZxkngT6L5JQ9WUQ2UnztS6hqyfcQiFqvk0yhcvWsrZLJNiiDZcKzGbLfJK2sDy7tSt9uWdP1cIgMHIkaw==',
};

export function verifyRelease(metadata: RegistryMetadata | null | undefined, name: string, version: string, now = Date.now()): { url: string; integrity: string } {
  const entry = metadata?.versions?.[version];
  const published = Date.parse(metadata?.time?.[version] as string);
  const pinned = name === '@openai/codex' && Object.hasOwn(codexIntegrity, version) ? codexIntegrity[version] : null;
  if (!entry || entry.name !== name || entry.version !== version || !Number.isFinite(published) || published > now ||
      (pinned ? entry.dist?.integrity !== pinned : now - published < 14 * 86400000))
    throw Error(`${name} の固定版・公開後14日の条件を確認できません`);
  const url = new URL(entry.dist?.tarball as string);
  if (url.origin !== 'https://registry.npmjs.org' || !url.pathname.startsWith(`/${name}/-/`) || url.search || url.hash || !/^sha512-[A-Za-z0-9+/]+={0,2}$/.test(entry.dist?.integrity ?? ''))
    throw Error(`${name} の公式配布元・SHA512を確認できません`);
  return { url: url.href, integrity: entry.dist!.integrity! };
}
export function verifyArchive(data: Buffer, integrity: string): void {
  if (`sha512-${createHash('sha512').update(data).digest('base64')}` !== integrity)
    throw Error('CLI配布物のSHA512が一致しません');
}
async function download(url: string, max: number): Promise<Buffer> {
  const response = await fetch(url, { redirect: 'error', signal: AbortSignal.timeout(120000) });
  if (!response.ok) throw Error('公式CLI配布元から取得できません');
  const data = Buffer.from(await response.arrayBuffer());
  if (data.length > max) throw Error('CLI配布物が上限を超えました');
  return data;
}
async function install(): Promise<void> {
  const versions: ImageVersions = JSON.parse(fs.readFileSync(new URL('./versions.json', import.meta.url), 'utf8'));
  const wanted: [name: string, version: string][] = [['pnpm', versions.pnpm], ['@openai/codex', versions.codex], ['@anthropic-ai/claude-code', versions.claude]];
  const platform = process.arch === 'x64' ? 'x64' : process.arch === 'arm64' ? 'arm64' : null;
  if (process.platform !== 'linux' || !platform) throw Error('CLI配布はLinux amd64/arm64だけです');
  const checks: typeof wanted = [...wanted, ['@openai/codex', `${versions.codex}-linux-${platform}`], [`@anthropic-ai/claude-code-linux-${platform}`, versions.claude]];
  const cache = new Map<string, RegistryMetadata>();
  const archives: string[] = [];
  const dir = fs.mkdtempSync('/tmp/agent-tools-');
  try {
    for (let i = 0; i < checks.length; i++) {
      const [name, version] = checks[i];
      if (!cache.has(name)) cache.set(name, JSON.parse((await download(`https://registry.npmjs.org/${encodeURIComponent(name)}`, 64 << 20)).toString('utf8')));
      const release = verifyRelease(cache.get(name), name, version);
      const data = await download(release.url, 256 << 20);
      verifyArchive(data, release.integrity);
      const file = path.join(dir, `${i}.tgz`);
      fs.writeFileSync(file, data, { mode: 0o600 });
      // Native optional packages are verified too; npm resolves their fixed versions and integrity.
      if (i < wanted.length) archives.push(file);
    }
    execFileSync('npm', ['install', '--global', '--no-audit', '--no-fund', ...archives], { stdio: 'inherit' });
    execFileSync('npm', ['cache', 'clean', '--force'], { stdio: 'inherit' });
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
}
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) await install();
