# issue-desk-worker

> **このリポジトリについて**
> 個人で設計・運用している「AIに開発を任せる仕組み」issue-desk の実行側です。各PCに常駐し、受付 [issue-desk-intake](https://github.com/mofua-suki/issue-desk-intake) から仕事を受け取って、AI（Codex / Claude）による実装・固定テスト・別の実行先による独立レビュー・PR公開・マージまでを進め、結果と使用量を報告します。lease による仕事の所有確認、中断・再開、同時実行枠の制御を備えています。
>
> - 構成: Go（標準ライブラリ中心）、Docker
> - 公開用に整理した版です。開発履歴は1コミットにまとめています。


各実行PCで動くWorkerのGo実装です。受付のServer・ブラウザ画面・DB操作は含みません。

## 構成

- `cmd/worker`: 起動と設定の確認。
- `cmd/hostd`: ホスト上でのWorker更新と実地確認。
- `cmd/review-counts`: 完了結果からレビュー件数を集計。
- `cmd/desk`: 指定した受付APIの診断・読取・操作。
- `internal/config`: 3項目の設定、HTTPS接続先、PC別トークンファイルの検査。
- `internal/intake`: 設定取得、heartbeat、仕事取得、lease更新、状態報告のHTTP API。
- `internal/execution`: AI実装・固定テスト・独立レビュー・公開・マージと依頼キュー。
- `internal/journal`: 工程と送信結果の永続化。
- `internal/webhook`: 認証済みWebhookの起動合図。
- `internal/runtime`: 仕事の生存期間、lease喪失時の中断、公開前の所有確認、終了処理。

## 設定

```json
{
  "serverUrl": "https://issue-desk.example",
  "serverTokenFile": "private/intake-token",
  "workerId": "worker-01"
}
```

`serverUrl`は認証情報・パス・クエリ・フラグメントを含まないHTTPS URLです。`workerId`には受付に登録された、英数字で始まる英数字・`_`・`.`・`-`の1〜64文字の担当名を指定します。設定ファイルは上記の3項目だけを受け付けます。トークンファイルの相対パスは設定ファイルの場所を基準に解決し、所有者以外が読めるファイルは拒否します。資格情報をGitへ追加しないでください。

## ビルドと確認

```sh
make check
make build REVISION="$(git rev-parse HEAD)"
./bin/worker version
./bin/worker check --config /絶対パス/config.local.json
```

`REVISION` の既定値は `development` です。`make check` は gofmt の整形確認、deadcode v0.52.0・staticcheck v0.8.2、`-race` 付きの `go test ./...` と `go vet ./...` を実行します（`-race` はcgoとCコンパイラーを要します）。

`check`は受付の設定APIを読み、PC別設定を取り出したうえで、Gitea監視アカウントとその権限、実行状態フォルダー、Codex/Claudeの契約認証、実際のsandboxによる資格情報・状態の隔離を検査します。heartbeatや仕事の取得は行わず、journalロックも取りません。全検査に合格すると設定の版と`executionReady: true`を表示し、トークンは表示しません。

## 常駐処理

```sh
./bin/worker run --config /絶対パス/config.local.json
```

受付から設定を読み、Codexの契約認証・必要なCLI・資格情報と状態領域の隔離を検査してから仕事を取得します。AI実装、固定テスト、独立レビュー、PR公開、マージの番、fast-forwardマージ、使用量と結果報告を管理します。独立レビューは通常は実装と反対の実行先を使いますが、実行先の一時無効により実装と同じ実行先になることがあります。停止・追加指示を受付APIから受け取り、実行中の子プロセスを中断します。

受付が配った仕事のleaseを更新し、公開前に所有権とマージの番を照合します。手動マージとブランチ指定のPR作成は、実装枠が埋まっていても処理できます。

- [取得の再試行・公開条件・依頼キュー](docs/worker-runtime.md)
- [生存・負荷・受け取り保留](docs/heartbeat.md)と[leaseの診断](docs/lease-diagnostics.md)
- [継続時に作業ツリーを確認できない場合](docs/continuation-cause-315.md)
- [レビュー件数の集計](docs/review-counts.md)

## 実行エラーの受付向け分類

受付の `view.lastExecutionError` とWorkerログを突き合わせるには、[分類値・HTTPステータスの対応](docs/worker-runtime.md#実行エラーの受付向け分類)を参照してください。

## desk CLI

Workerと同じ3項目の設定を使います。

```sh
./bin/desk doctor --config /絶対パス/config.local.json
./bin/desk get --config /絶対パス/config.local.json --path '/api/v1/client-config?host=実行PCの担当名'
./bin/desk operation --config /絶対パス/config.local.json --method POST --path /api/issues --body-file /絶対パス/request.json
```

`doctor`は設定APIを読み取るだけで、heartbeatや仕事取得を行いません。`get`は明示したAPIを1回読み取ります。`operation`はPOST・PUT・PATCH・DELETEとAPIパスを明示したときだけ送信します。`--body-file -`なら標準入力からJSONを読み取ります。引数に本文やトークンを直接渡す機能はありません。

接続先は設定済みServerの`/api/`配下に限ります。任意URL、画面のCookie、一時キー、独自の認証ヘッダーへの切り替えはできません。操作可否はServerが判断し、拒否された操作の別経路での再送は行いません。

送信ID（`id`・`requestId`など）は対象APIの契約に従って本文に指定します。本文のbytesとIDはそのまま送信し、IDの生成・置換・自動再送をしません。応答が届かなかった操作は、同じ送信IDの受付結果を読取APIで照合してから扱います。

応答は`status`と`body`を分けたJSONです。`bodyType`は`json`・`text`・`empty`、読取不能なら`unavailable`です。トークン・秘密に該当する応答値は伏せ、`redacted: true`を付けます。JSONの数値を浮動小数点に変換しません。HTTP転送も元のstatusを表示し、転送先へ追従しません。

終了コードはHTTP 2xxが0、HTTPによる拒否・失敗・転送が1、ローカル設定・引数・通信の失敗が2です。応答を得たHTTPエラーと、応答を得られない通信失敗を区別します。

## hostd

登録済みのWorker版を各PCへ即時反映するには `hostd update-now --config /絶対パス/hostd/config.json --wait 10m` を使います。登録と各PCの切替は別です。終了コードと待機・復旧の扱いは [hostd更新手順](docs/hostd-update.md) を参照してください。通常の5分周期は変更しません。

`cmd/hostd`はホスト上で1周期を実行します。Workerの3項目設定とは別に、既存のhostd設定・専用の`idh_`トークン・registryの読取専用パスワードを使います。トークンとパスワードは設定ファイルと同じフォルダーの`token`と`registry-password`に0600で置きます。

```sh
CGO_ENABLED=0 go build -trimpath -o bin/hostd ./cmd/hostd
./bin/hostd --config /絶対パス/hostd/config.json --check
./bin/hostd --config /絶対パス/hostd/config.json
```

`--check`は設定・資格情報・同時実行ロックを検査し、APIやDockerは呼びません。別の周期や即時反映がロックを持っているときは、設定と資格情報を確かめたうえで「実行ロック中です」と表示して0で終えます（user serviceの`ExecStartPre`を失敗にしないため）。通常は既存のホストタイマーから5分ごとに呼びます。設定の`url`はHTTPS、`host`は自分の担当名、`clientDir`は`compose.yaml`・`.env`・`update-linux.sh`を置く絶対パスです。

更新は新しい引取の停止と新鮮なidle heartbeatを確認し、指定されたタグ・digestの起動を確認します。失敗時は前の環境へ戻します。工程と結果を原子的に保存し、結果送信の再試行で更新を繰り返しません。Dockerの環境失敗は版の不具合と区別して報告します。

Linux hostdの`install`指示は、先に版を取得し、引き取り停止の応答と新鮮なheartbeatを確認した後、公開中の仕事が残っていてもWorkerをSIGTERM・猶予60秒で停止します。コンテナの停止を確認してから指定版へ切り替え、稼働コンテナのdigestと新版のready heartbeatを確認します。失敗時は直前の環境へ戻し、環境変更前の失敗でも停止した旧Workerを起動し直します。リースを返すAPIは呼ばず、保存済み工程・結果・未送信使用量の照合と再開は期限切れ後の再配布に委ねます。指示と未送信結果はhostdの状態領域へ原子的に保存し、同じIDを二度実行せず同じ結果を再送します。`rollback`・`pin`・`follow`指示は`unsupported`です。installによる版の固定は行わず、次の通常更新では受付のdesiredへ追従します。

アプリの配置はローカル`apps`一覧の`repo/path/workDir/composeFiles/envFile/before/after/health`だけを使います。checkout後にsymlinkと実パスを検証し直し、起動確認に失敗した配置を前のコミットへ戻します。`readers`の`repo/path/checkCommand`はcleanな既定枝だけをfast-forwardします。`liveChecks`の`repo/path/command/timeoutSeconds`と遠隔依頼が完全一致した場合だけ実地確認を実行し、16KB以内の数値指標と検査結果だけを報告します。任意の遠隔コマンドは実行しません。

NASホストの実地確認は`hostd live-check --config <live-check-host.jsonの絶対パス>`で実行します。単独設定の各`liveChecks`項目に`"checkout": true`を指定すると、cleanな専用Git作業ツリーで`origin`の既定ブランチを取得し、そこから到達できる依頼コミットをdetachedで取り出してから実行します。追跡対象外ファイルを含む未コミット変更がないこと、`origin`が対象リポジトリを指し、PC側のGit認証で取得できることが必要です。取得・到達確認・取り出しに失敗すると`checkout_failed`を返します。未指定ではHEADが依頼コミットと一致する必要があります。url/host/tokenFile/stateFile/liveChecksと保存済み{id,result}、0600の本人Bearerを使い、registry-passwordを要求しません。NASの既存Codex bindはCODEX_AUTH_DIR、Webhook公開先はWEBHOOK_BIND/WEBHOOK_PORTで.envに指定できます。未指定ならCodex認証はvolume、Webhookはloopback公開です。

### macOSの単独実地確認

Apple SiliconのmacOSで、別途用意した実行可能なMach-O arm64版hostd、Python 3、Gitと、受付へのHTTPS接続を使います。作業ツリーのoriginとGit認証を事前に用意し、既存の受付側経路で発行したトークンを標準入力に1行入力します。管理者権限やDockerは不要です。

```sh
bash deploy/client/macos-live-check/install.sh /絶対パス/hostd PC名 https://受付URL owner/repo /絶対パス/作業ツリー 300 true コマンド 引数
```

最後の引数以降はコマンド配列です。`true`はcheckoutを有効にし、`false`は無効にします。空白を含む引数はシェルで引用してください。LaunchAgentは対話シェルのPATHを引き継がないため、コマンドには実行ファイルの絶対パスを指定し、コマンド内で呼び出すツールも絶対パスを使うか、コマンド内でPATHを設定してください。実地確認ではcheckoutの設定にかかわらずGitを呼び出すため、LaunchAgentのPATHからGitを実行できる必要があります。設定・トークン・状態は `~/Library/Application Support/issue-desk/live-check` に保存され、利用者のLaunchAgentが5分ごとに実行します。標準出力と標準エラーは `/dev/null` に破棄されます。登録状態は `launchctl print gui/$(id -u)/com.issue-desk.live-check` で確認できます。停止するには `launchctl bootout gui/$(id -u)/com.issue-desk.live-check` を実行します。

iOS simulatorを使う確認はMac上で実行し、作業ツリーとコマンドを実地確認用に設定してください。

## Workerイメージ

`Dockerfile`は既存の固定AIツールイメージを`TOOLS_IMAGE`で指定して作ります。最終イメージへコピーするのはAIツール、Goツールチェーン、Worker/desk/hostdバイナリ、起動スクリプト、Workerの局所導入・受付窓口・認証用の配布資産です。Server・Client・DBコードは含みません。

```sh
docker build --build-arg TOOLS_IMAGE=固定されたツールイメージ --build-arg ISSUE_DESK_REVISION=40桁のコミット -t 配布先/issue-desk-client:タグ .
```

Pythonが必要な実行先は`--target client-python`を指定します。`deploy/compose.yaml`と`deploy/update-linux.sh`は既存のregistryタグ、サービス名、volumeを使います。WebhookはWorkerが直接受け、更新スクリプトは旧Webhook入口コンテナを停止してからWorkerを起動します。資格情報と状態volumeを保持して更新し、volumeは削除しないでください。旧greenと共有volumeは自動削除しません。

hostdはイメージだけを更新し、配置済みのcomposeや更新スクリプトは置き換えません。そのため旧composeの入口に向けて、互換用の`worker webhook-proxy`がWorkerへ転送します。新しい配布資産へ切り替える前に、greenが稼働していないこととWorkerのWebhook待受がコンテナ外から到達できることを確認してください。更新スクリプトは旧hostd向けの互換引数も受け付けます。

## 局所導入と診断

起動時は受付の設定、Gitea監視アカウントと権限、実行状態の保存、CLIの版と契約認証、資格情報・状態の実隔離、保存済みの仕事の復旧、Webhook待受を確認します。`worker check` の内容は「ビルドと確認」を参照してください。起動不成立時はheartbeatに `ready=false` と失敗項目を送り、仕事を取得しません。

### 監視WorkerのCodex再ログイン

[Codex再ログイン手順](docs/codex-login.md)で対象Workerの認証保存先を確認し、同じ保存先でログインと起動検査を行います。Claudeの利用上限時に使うキーの設定は[Claude APIキーの配置と更新](docs/api-key.md)を参照してください。

### トークンとホストの導入

`worker token-place --config /state/config.local.json --phase existing|check|write|verify --kind worker-token --worker-id 担当名`は標準入力の既存GiteaトークンをworkerAccountsの所有者と照合します。writeではstate/private/worker-tokenへ0600で原子的に置換します。トークンを発行・失効しません。両色へ置く場合は両方をcheckした後でwrite・verifyします。

`deploy/client/linux-hostd/install.sh`は配置先・担当名・HTTPS受付URL・Workerイメージ・任意のhostd配置先を引数で受け、既存hostdトークンとregistryパスワードの2行を標準入力で受け取ります。新バイナリ・設定の検査を済ませ、既存の制限・apps/readers/liveChecksを保ったまま5分周期のcrontabへ置きます。NAS側の資格情報発行、OS導入、全PCへの導入順序はIntakeの導入処理が管理します。

NASのようにcrontabの更新が許されない既存systemdホストでは、権限を変更せず`deploy/client/linux-hostd/issue-desk-worker-hostd.service`と同名の`.timer`をuser unitとして使います。標準配置は`~/.local/share/issue-desk/worker/hostd/hostd`と同じフォルダーの`config.json`・既存`token`・`registry-password`です。Worker imageから抽出したバイナリを`--check`してから、2つのunitを`~/.config/systemd/user/`へ配置し、`daemon-reload`後にserviceを1回実行します。`systemctl --user show issue-desk-worker-hostd.service -p Result -p ExecMainStatus`と受付のhostdHealthで実周期の成功を確認してからtimerを有効化します。timerは完了後5分周期です。既存のcron周期がある場合は同時起動せず、そのhostd行だけを保留します。

unitはそのプロセスだけ`env -u GODEBUG`で起動します。cron installerでcrontab更新が拒否された場合は非0で終了し、`linux_hostd_installed`を出しません。バイナリと資格情報の配置だけを周期登録成功と扱わず、systemdの実周期とtimerの次回時刻まで確認してください。

チャット窓口の配布はIntakeのdeploy/clientsが管理します。`deploy/client/login-antigravity.sh`は同じcomposeの認証volumeに対して、保護設定をGo入口で準備してからログインします。Linuxの更新はホストのPython 3を使い、Node.jsはWorkerイメージ内の固定版を使います。

## 固定テスト

[テスト環境](docs/test-environment.md)に一時領域・依存の取得・ブラウザ試験の実行条件を、[実行証拠と検証診断](docs/execution-evidence.md)に結果と再実行の扱いを記載しています。

## 独立した固定ツールの構築

ツールイメージの作成・版の検証・Workerへの受け渡しは[固定ツールの構築](docs/test-environment.md#独立した固定ツールの構築)を参照してください。

## 作業ツリーへの反映・通知・追加指示

controllerのdeleteFilesとClaudeの`.claude-staged`は一覧全体を検証してから適用し、リンク・作業ツリー外・Gitメタデータ・worktrees・競合解消の許容範囲外を拒否します。親dirfdを固定して経路差替でも作業ツリー外へ作用せず、反映履歴をjournalへ保存します。stageはcompleted後だけ反映し、コミットしません。

Issue・PRへの投稿（outbox、承認時の後続案、再投稿コマンド）の扱いは [Issue・PRへの投稿](docs/issue-comments.md) に記載しています。

画面/APIの追加指示はcommand ID付きで保存し、text/contextを次の実装とレビューへ渡します。停止を優先しても指示は保持し、Serverの新Descriptorに同じIDがあれば二重に積みません。取得済みleaseがExecuteに登録される前の指示をstaleとして消費しません。poll周期はassignments.pollSeconds（未指定300秒）です。認証済みWebhookはschedulerを起こします。

## 報告と使用量

[実装報告と使用量](docs/execution-evidence.md#実装報告と使用量)に、報告の補完、回数上限、中断時の使用量、範囲縮小の扱いを記載しています。

## Appleネイティブ検証

Repositoryのoptional `nativeTestHost`を設定すると、Issueと手動PRのマージ前に公開済みhead・現在のbase・マージ予定treeを固定した`native_test`を配布します。固定IDで結果を確認し、`passed`と全SHAの一致を必須にします。失敗・未実行・期限切れ・設定変更・lease/merge turn喪失時はマージを保留します。保留した仕事は実装の修復再試行へ戻しません。未設定のリポジトリではネイティブ検証を要求しません。

`assignments.hostRoles[host]`（無ければ設定トップの`hostRole`）は既定`implementation`または`test-only`です。後者は`repositories`を空にし、実装・PR保守を取得できません。`testRepositories`で許可した対象だけを独立した`worker apple run --config PATH` / `worker apple check --config PATH`が検証します。設定はWorkerと同じ `serverUrl`・`serverTokenFile`・`workerId` の3項目です。Macの別ID登録・Docker DesktopとLaunchAgentの準備は[Appleホストの導入手順](deploy/client/macos-apple/README.md)を参照してください。
