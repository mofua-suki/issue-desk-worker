// JVM・Flutter のリポジトリの固定テスト用に、公式の Linux x86_64 配布物を固定版・SHA256 照合で /opt へ入れる。
import fs from 'node:fs';
import { execFileSync } from 'node:child_process';

import type { ImageVersions } from './image-versions.mts';

type SdkName = 'jdk' | 'gradle' | 'flutter';
type SdkRelease = { url: string; sha256: string };
export type SdkReleases = Record<SdkName, SdkRelease>;

const releases: SdkReleases = JSON.parse(fs.readFileSync(new URL('./sdk-release.json', import.meta.url), 'utf8'));
const versions: ImageVersions = JSON.parse(fs.readFileSync(new URL('./versions.json', import.meta.url), 'utf8'));

/** 配布物の URL が定義の版と公式の配布元を指すか。 */
export function checkSdkRelease(name: SdkName, release: SdkRelease, version: string): void {
  const origins = {
    jdk: ['https://github.com', '/adoptium/temurin25-binaries/releases/download/'],
    gradle: ['https://services.gradle.org', '/distributions/'],
    flutter: ['https://storage.googleapis.com', '/flutter_infra_release/releases/stable/linux/'],
  };
  const url = new URL(release.url);
  const [origin, prefix] = origins[name] ?? [];
  if (url.origin !== origin || !url.pathname.startsWith(prefix)) throw Error(`${name} の公式配布元を確認してください`);
  if (!new RegExp(`[^0-9.]${version.replaceAll('.', '\\.')}[^0-9.]`).test(url.pathname)) throw Error(`${name} の配布物が versions.json の版と違います`);
  if (!/^[a-f0-9]{64}$/.test(release.sha256)) throw Error(`${name} の SHA256 を確認してください`);
}

function fetchVerified(name: SdkName): string {
  const release = releases[name];
  checkSdkRelease(name, release, versions[name]);
  const archive = `/tmp/${name}-sdk`;
  execFileSync('curl', ['--fail', '--silent', '--show-error', '--location', '--proto', '=https', '--proto-redir', '=https', '-o', archive, release.url]);
  const hash = execFileSync('sha256sum', [archive], { encoding: 'utf8' }).split(' ')[0];
  if (hash !== release.sha256) {
    fs.rmSync(archive, { force: true });
    throw Error(`${name} の SHA256 が一致しません`);
  }
  return archive;
}

function install(name: SdkName, dest: string, extract: (archive: string, dest: string) => void): void {
  const archive = fetchVerified(name);
  try {
    fs.rmSync(dest, { recursive: true, force: true });
    fs.mkdirSync(dest, { recursive: true });
    extract(archive, dest);
  } finally {
    fs.rmSync(archive, { force: true });
  }
}

if (process.argv[1] && fs.realpathSync(process.argv[1]) === fs.realpathSync(new URL(import.meta.url).pathname)) {
  if (process.arch !== 'x64') throw Error('JDK・Gradle・Flutter の固定配布物は Linux x86_64 用です');
  install('jdk', '/opt/jdk', (a, d) => execFileSync('tar', ['-xzf', a, '-C', d, '--strip-components=1', '--no-same-owner']));
  install('gradle', '/opt/gradle', (a, d) => {
    execFileSync('unzip', ['-q', a, '-d', '/tmp/gradle-unpack']);
    execFileSync('sh', ['-c', 'mv /tmp/gradle-unpack/gradle-*/* "$1" && rm -rf /tmp/gradle-unpack', 'sh', d]);
  });
  install('flutter', '/opt/flutter', (a, d) => execFileSync('tar', ['-xJf', a, '-C', d, '--strip-components=1', '--no-same-owner']));
}
