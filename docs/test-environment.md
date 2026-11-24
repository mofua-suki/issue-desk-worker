# 固定テストとツールイメージ

固定テストの結果と診断は受付へ報告します。証拠の条件と再実行の扱いは [実行証拠と検証診断](execution-evidence.md) を参照してください。

## 一時領域と容量不足時の再実行

固定テストは実行ごとに0700の専用一時領域を作り、Go/XDG cacheをその下に置いて終了時に削除します。出荷Composeは512 MiBのtmpfsを `/sandbox-tmp` に用意します。起動環境の `ISSUE_DESK_SANDBOX_TMPFS_DIR` に書き込み可能なtmpfsディレクトリの絶対パスを指定すると、空き容量が16 MiB以上ある場合に使用します。未指定・利用不能・容量不足の場合は `<RuntimeDir>/workspaces` に作成します。

tmpfs使用中に容量不足が報告された場合はディスク上の新しい領域で最大1回実行し直します。容量不足と判断するのは、テストのコマンドの実行が空き不足のerrno（`ENOSPC`・`EDQUOT`）で失敗した場合か、中断されずに失敗したコマンドの標準エラーに、前後の空白を除いて `no space left on device`・`disk quota exceeded` だけの行か、`: no space left on device`・`: disk quota exceeded` で終わる行（大文字と小文字は区別しません）がある場合だけです（標準出力の引用や文中の言及では実行し直しません）。別の領域の容量不足や、テストが標準エラーにその診断を出すだけの失敗でも実行し直すため、時間が増えることがあります。再実行ではコマンドの作業ツリーへの変更が残る場合があります。HOME/資格情報は開きません。

## 依存の取得とキャッシュ

Go依存は公式proxy/sumdbと配布物redirect先だけを許可し、GOTOOLCHAIN=localでSDKの自動切替を防ぎます。

JVM・Flutterのリポジトリ向けに、`GRADLE_USER_HOME`・`PUB_CACHE`・`java.io.tmpdir`（`JAVA_TOOL_OPTIONS`）も一時領域の下へ向け、`JAVA_HOME=/opt/jdk`を渡します。通信先は`repo.maven.apache.org`・`pub.dev`（配布物のredirect先`storage.googleapis.com`）を足し、書き込み先はFlutterが毎回印を書き直す`/opt/flutter/bin/cache`だけを足し、その中のSDK・成果物のディレクトリと`*.snapshot`には読み取りの指定を重ねます（イメージにFlutterがある場合だけ。codex sandboxの中で差し替えを止められるかは未確認）。Gradle・Kotlinのdaemon抑止は`GRADLE_OPTS`で指定します（GradleがJVM設定のために起動する単発daemonはビルド終了時に停止します）。Gradleはイメージの`gradle`をtestCommandに使い、wrapperの配布物の取得（services.gradle.org）とGradle Plugin Portal（plugins.gradle.org）は許可しません。プラグインの取得先にも`mavenCentral()`を指定してください。AIセッションも同じ環境変数と許可を使います。

setup を実行する作業ツリーでは、未追跡の `node_modules/` を Worker の Git 操作で除外します。追跡済みファイルの変更は除外しません。`pnpm-lock.yaml` は setup 前に存在せず未追跡で、setup が生成した場合に限って除外し、追跡済みや既存の lockfile の変更は通常どおり扱います。除外情報は作業ツリーごとの Git 管理領域に保持します。

入れ子clientの依存は設定testCommand側で`pnpm --dir client install --frozen-lockfile --ignore-scripts --ignore-pnpmfile --config.minimum-release-age=20160`を行えます。全階層の依存設定をその前に検査します。

JVMの通信補助処理は固定テストと書き込み可能なAIセッションで共通です。`ToolchainEnv`はイメージのJDKで依存なしのJava agentを構築し、実行ごとの一時領域へ配置して`JDK_JAVA_OPTIONS`で読み込ませます。JVM本体・Gradleの子JVM・Kotlinの子JVMは、それぞれsandbox内で起動するときに`HTTP_PROXY`・`HTTPS_PROXY`（小文字も対応）を読み、HTTP proxyのシステムプロパティとProxySelectorへ設定します。片方しかない場合は両方に使います。ホスト側のproxy変数の継承は前提にしません。`NO_PROXY`は適用せず、`localhost`・`127.0.0.0/8`・`::1`のリテラル宛てだけ直接接続します。その他の宛先はproxyの拒否・接続失敗時に直接接続へ切り替えません。proxyは認証情報を含まない`http://host[:port]`形式に限定し、未提供・不正な設定やagentの準備失敗ではJVM起動を失敗させます。proxy値は診断出力へ出しません。許可通信先の判断は既存のsandbox proxyが行い、JVM補助処理は通信先を追加しません。agentの構築にはイメージの`/opt/jdk/bin/javac`が必要で、構築物もキャッシュもHOMEへ保存しません。

## 受付のブラウザ試験

ブラウザ試験には、GUI用のプロファイル共有ソケットを作らない `chromium-headless-shell` の実バイナリを直接使います。Debianの `/usr/bin/chromium-headless-shell` は子プロセスを `exec` しないため、ブラウザの終了を直接制御できる `/usr/lib/chromium/chromium-headless-shell` を起動します。固定テストのUnixソケット作成拒否と、外部通信のドメイン制限は維持します。

`mofua-suki/issue-desk-intake`の固定テスト環境には、両方の実行ファイルが利用可能で、固定テストと同じsandbox内でChromiumの起動検査が成功した場合に、`ISSUE_DESK_TEST_CADDY=/opt/agent-tools/bin/caddy`と`ISSUE_DESK_TEST_CHROMIUM=/usr/lib/chromium/chromium-headless-shell`を渡します。起動検査が失敗した場合は両変数を設定せずに固定テストを続行し、ブラウザ試験を省略した理由を結果の詳細に記録します。他のリポジトリには渡しません。

## 独立した固定ツールの構築

`Dockerfile.tools`は[deploy/versions.json](../deploy/versions.json)を版の正本とし、Node・pnpm・Codex・Claude・Antigravity・Caddy・JDK（Temurin）・Gradle・Flutterを構築します。JDK・Gradle・Flutterは`deploy/sdk-release.json`の公式配布元・固定URL・SHA256で`deploy/install-sdks.mts`が`/opt/jdk`・`/opt/gradle`・`/opt/flutter`へ入れ（Linux x86_64だけ）、Flutterは`flutter test`に要る共通の成果物を構築時に取得し、ビルド時に3つの版表示を正本と照合します。CaddyはGoモジュールの既定のチェックサムデータベースで取得物を検証して構築し、ビルド時に版表示を正本と照合します。WorkerイメージにはDebianの署名検証付きaptからChromium Headless Shellを導入し、非rootでヘッドレス起動を確認します。`bash deploy/build-tools.sh`はcleanな固定commitから`issue-desk-tools:<SHA先頭12桁>`を作り、そのtagを`TOOLS_IMAGE`としてWorkerとIntakeのbuildへ渡せます。registryへ発行する場合はtagに加えてregistryのmanifest digestを固定してください。docker save/loadではOCI index digestとconfig IDの表示が異なる場合があるため、archiveのSHA256とmanifest.jsonのConfigも照合します。

Nodeは公式SHASUMS256、Antigravityは固定SHA512と公開14日、npm CLI本体とLinux native配布物は公式registryの固定版・公開14日・SHA512を照合します。公開14日未満の例外は [deploy/install-agent-tools.mts](../deploy/install-agent-tools.mts) に列挙した `@openai/codex` の版と配布物に限り、各SHA512の一致と公開日時が未来でないことが条件です。非公式tarball・内容改竄はbuildを止めます。版をbuild argsで上書きせず、変更は定義ファイルのレビューを経ます。

toolsとWorkerの両イメージは、Flutter SDKの所有者と異なるuidで`--flutter-readonly`を必須検査として実行します。`/opt`は読み取り専用相当とし、`bin/cache`直下の印とロックだけを更新可能にして、版表示と最小パッケージのテスト、保護対象の内容を検査します。書き込み可能なcacheの親からのsnapshotの置換は完全には防げません。Gitのsystem設定では`/opt/flutter`だけを`safe.directory`に指定します。
