# Appleネイティブ検証ホスト

テスト専用Macは受付画面で別PCとして登録し、`role: test-only`、空の`repositories`、対象の`testRepositories`を設定する。リポジトリの`nativeTestHost`をそのIDにし、**そのホストへ配布する**`fleet.repositories[].testCommand`（コマンド配列）と`testTimeoutSeconds`を設定する。例は `["/bin/sh", "scripts/verify-apple.sh", "macos"]`。payloadからコマンドやURLは採用しない。

発行トークンの対話操作は既存受付画面で行う。K2は`serverUrl`・`serverTokenFile`・`workerId`だけで、トークンは所有者だけが読める既存PCトークンファイルを使う。秘密・K2・runtimeをGitへ入れない。Git監視アカウントは登録された本人と許可された各リポジトリの読取権限だけを検査する。Issue/PRの書込scopeは不要。権限不足を自動修正しない。

Mac上でDarwin用バイナリを用意する（ビルド環境では `GOOS=darwin GOARCH=arm64 go build -o /外部の配置先/worker ./cmd/worker`。Intel Macは`amd64`）。Xcode、必要なSDK、Flutter等は利用者が事前に導入し、sandboxから読める`/Applications`・`/Library/Developer`・`/opt/homebrew`等へ配置する。test-onlyにCodex・Claude・Dockerは不要。

```sh
bash deploy/client/macos-apple/prepare.sh --start --role test-only \
  --config /外部の設定先/apple-k2.json --worker /絶対パス/worker
/絶対パス/worker apple check --config /外部の設定先/apple-k2.json
```

スクリプトは利用者の`~/Library/Application Support/issue-desk/apple-prepared/<workerId>`へLaunchAgentを準備する。--startを指定するとcheck成功後にlaunchctlへ登録して起動する。省略時は準備だけ行う。既存plistと登録済みWorkerは上書きしない。診断ログは生成先のlogsへ保存する。権限取得・トークン発行・Docker設定変更・既存Workerの置換はしない。`worker apple run --config PATH`で前景起動もできる。`worker apple probe --config PATH --repo OWNER/NAME --tree DIR`は、隔離の検査の結果を表示したうえで（失敗しても続けて）、配られた固定testCommandを要求と同じ隔離の中で、指定した作業ツリーに対して1回流し、出力と終了コードを返す。検証要求の引き取りと受付への結果の送信はしない。隔離の規則を直すときの確認に使う。通常Workerの引き取り停止と実行・送信の完了を確認してから置き換える。

実装も担当するMacは、受付で **<name>-test と <name>-worker の別ホストID・別account・別PC token** を登録する。Linux用IDは`role: implementation`と既存`repositories`のrank・implementersを設定し、Docker Desktop上でLinux Workerを使う。Apple用IDは`test-only`で上記と同じ設定にする。配布先の登録は受付側の登録生成で`deploy/nas/worker-release.json`のhostsへ追加する。スクリプトは登録やトークン発行を行わない。

初期化の前提はDocker Desktop・python3・Darwin Workerの導入と、本人が用意した以下のファイルである。Docker Desktopの初回利用規約は本人が承認し、`docker context show`が`desktop-linux`であることを確認する。別contextをスクリプトが変更することはない。非公開registryを使う場合は、本人がDocker Desktopで配布registryへのログインを済ませる。既存のDocker認証設定は変更しない。

- Apple K2とLinux K2。どちらも3項目のみ。Linux K2の`serverTokenFile`は`/state/private/pc-token`、配布するLinux設定の`workerTokenFile`は`/state/private/worker-token`にする。
- Linux用PC token、Linux accountのGitea token、Linuxホスト用 **host image token** の各ファイル。host image tokenはPC tokenと別物であり、`/api/v1/hosts/<name>-worker/desired-image`の認証に使う。受付・GiteaのURLは本人の登録済みURLを使用し、画面で本人専用のトークンを発行する。
- 本人がログイン済みのCodex `auth.json`、Linuxで利用できるClaude `.credentials.json`。秘密ファイルは本人所有で他ユーザーに読取権限のないものを用意する。スクリプトは既存ファイルの権限を変更しない。

AIログインは本人が行う。[Codexの公式認証手順](https://learn.chatgpt.com/docs/auth?surface=cli)では`codex login --device-auth`を実行し、表示されたコードを[デバイス承認画面](https://auth.openai.com/codex/device)へ入力する。ファイル保存の認証を使い、本人の`auth.json`を指定する。OSキーチェーンだけに保存されている場合は、そのままファイル入力には使えない。[Claude Codeの公式認証手順](https://code.claude.com/docs/en/authentication)では`claude auth login`または初回の`claude`を実行し、表示される本人用ログインURLを開く。コードが表示された場合は端末へ戻してログインを完了する。Macのキーチェーンの内容を推測・抽出せず、本人がLinux CLIでログインして得た資格情報ファイルを用意する。ログイン・契約の承認とトークン発行は自動化しない。test-onlyにはこれらのAI認証もDockerも不要。

新しいMacでは、準備済みの本人専用ファイルのパスを指定して次の一回コマンドを実行する。値そのものを引数へ書かない。

```sh
bash deploy/client/macos-apple/prepare.sh --start --role implementation --flavor=-python \
  --config /本人専用の場所/mac-test-k2.json --worker /絶対パス/worker \
  --implementation-config /本人専用の場所/mac-worker-k2.json \
  --pc-token-file /本人専用の場所/linux-pc-token \
  --gitea-token-file /本人専用の場所/linux-gitea-token \
  --host-image-token-file /本人専用の場所/linux-host-image-token \
  --codex-auth-file /本人専用の場所/auth.json \
  --claude-auth-file /本人専用の場所/.credentials.json
```

`--compose`は省略時に同梱の`deploy/compose.yaml`を使う。flavorの既定は現在の配布と同じ`-python`。別flavorを明示する場合（標準版は`--flavor=`）、そのホストに登録された配布flavorと完全一致する必要がある。一致する配布がない場合や配布応答が不正な場合は診断して停止する。bootstrapはhostdと同じdesired-image契約（flavorと空のcurrentRevision）で現行のtag・registry・revision・digest・flavorを取得・検証し、digestで固定してpullする。Linux/amd64を明示し、PC別のstate/codex/cli/agy volumeを準備し、K2と秘密を標準入力経由で配置する。既存ファイルは内容が一致するときだけ保持し、不一致なら止める。古いタグや配布先の.envは使わない。

Docker Desktopが停止中なら起動を要求して最大2分待つ。Apple checkとLinux Worker checkが成功してからcompose起動・LaunchAgent登録を行う。PC別composeプロジェクトを使い、既存コンテナがある場合は起動せず診断する。所有不明のvolumeは採用しない。中断した初期化は生成先のdocker-bootstrap.jsonとvolumeの所有ラベルが一致し、既存ファイルが入力と同じ場合に再開できる。失敗時にvolume・秘密を削除しない。Linux起動後にLaunchAgent登録が失敗した場合も、既存コンテナを自動置換しない。引取り停止・実行と送信の完了を確認してから復旧する。

runnerは`~/Library/Caches/issue-desk/apple/<workerId>`を専用runtimeとし、全体ロックを取得する。取得した依頼は永続化し、再開時は同じ対象を確認する。実行中に停止した依頼は再実行せず`interrupted`を報告する。実行済み結果は再送できるが、claim・設定を再確認する。取得通信は再送しない。停止・claim喪失時は子プロセス群を停止する。

リポジトリごとの一時checkoutでbase/headをfetchし、`merge --no-commit --no-ff`のtree一致を確認してから固定コマンドを実行する。テストはAppleの`sandbox-exec`で専用HOME/一時領域へ隔離し、PC/Gitトークンとrunner状態へのアクセスを拒否する。システムSDK（`/System`、`/Library/Developer`等）、設定したSDKと当該checkoutを読み、書込みはcheckout・専用HOME（SDKコピーを含む）・リポジトリ別pub cacheに限定する。ホストSDKは読取専用とし、要求ごとに専用HOMEへ全SDKを独立コピーする。ハードリンクを使わず、PATHとSDKを指すコマンド引数をコピー側へ向け、終了時に削除する。nativeNetworkは省略falseで通信禁止、trueなら依存取得・アプリ通信・FlutterローカルVMサービスを含む通信を許可する。nativeToolRootsには実在するSDKの絶対パスを指定し、その要求用コピーのbinをPATHへ加える。例は/Users/実際のユーザー/development/flutter。/home配下のSDKにも対応する。ルート全体、HOME全体、資格情報・制御状態を覆うSDKパス、SDK内のどの場所からでもSDK外を指すsymlinkは拒否する。内部の絶対symlinkはコピー内へ付け替える。既存SDKやトークンの権限は変更しない。空HOMEは要求の固定testCommand全体で共有し、終了時に削除する。再利用pub cacheはリポジトリ別に制御状態・トークンから隔離する。コマンド・SDK・通信設定はpayloadから採用しない。隔離probeが通らなければ仕事を取得しない。通常Linux Workerの隔離要件は変わらない。

実装側の待機は最大1時間。依頼IDはリポジトリ・対象ホスト・全SHA・設定バージョン・送り手・attemptから固定する。同じ要求の再開では同一、Issueのgeneration更新・別manual request・担当PC変更では別IDになる。曖昧な送信結果を別IDで再送しない。設定変更前の合格結果を新設定で再利用しない。受付のGET応答は既存の`request`包みと`status`を使い、`claimed`/`running`の依頼だけを実行・結果送信する。キャンセルや期限切れを確認した依頼のローカル状態は完了扱いとし、結果を公開せず次の依頼へ進む。

受付configのhostRoleはimplementation/test-only、testRepositoriesは現在ホストの文字列配列。assignments.hostsも文字列配列であり、ホスト別の役割とテスト担当はassignments.hostRoles[host]、assignments.testRepositories[host]から読む。他ホストのnativePolicyに送り手自身の値を流用しない。

受付のnativeTestVersions[Mac ID]はMacの有効fleet設定全体のdigestであり、Linux自身のconfigurationVersionとは異なる。Issue/manual双方のマージ番acquireとrenewにはnativeTestGate={version:1,host:対象MacID,configurationVersion:nativeTestVersions[対象MacID]}を付ける。16桁hexの配布値をそのまま使う。検証待機中とマージ直前にleaseとマージ番を再照合し、409や番の喪失後はマージしない。

Check成功後はreadyとheartbeatを送り、実装数は0、テスト中はbusyを報告する。CPU負荷は計測できなければ未知として扱う。終了・起動失敗時はready=falseを送る。古い設定の永続依頼はreleaseの成功または409を確認して終了し、結果を新しいclaimへ引き継がない。同じIDが再配布された場合も最新claimのpayloadからhead/base/treeを固定して新しい試行を保存する。503や結果不明では状態を保持する。

Apple sandboxのfile・mach・IPC・network要件とFlutter/Xcodeの実動作は、対象Macでの検証が必要。
