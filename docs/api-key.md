# Claude APIキーの配置と更新

キーの発行、課金設定、配備は運用者自身が行います。

## キーの渡し方と優先順位

Workerはサブスクリプションの利用上限で止まった間だけ、Claudeの呼出しにAPIキーを使います。キーは次の順で探し、どちらも無ければキーなしです。

1. このPCに置いたキーのファイル（以下の「配置前の確認」以降の手順）。置いてあればそれを使い、受付には問い合わせません。
2. 受付がNASで1回登録したキー（受付側の `deploy/nas/set-claude-api-key.sh set`）。Workerは自PCのPC別トークンで `GET /api/v1/hosts/{workerId}/claude-api-key` を読み、キーをメモリにだけ置きます（ファイルへは書きません）。読み直しは5分ごと、読取失敗の後は1分後です。消去（`cleared`）・未登録・読取失敗・形式違いはキーなしとして扱うため、受付で消去すると遅くとも5分で使われなくなります。この経路を持たない受付では、常にキーなしです。

NASで登録する場合、各PCでの作業は不要です。以下はPCごとにファイルを置く場合の手順で、NAS以外の実行PCで行います。

## 利用前の費用確認

Claude Consoleで対象組織とworkspace `issue-desk`を本人が確認してから、そのworkspaceでキーを作成してください。Consoleの請求・利用制限画面で月の支出上限がAPI利用に適用される範囲を確認し、付属クレジットの有無・額・期限、購入済み残高、auto-reloadの状態をそれぞれ確認します。購入済み残高は付属クレジットではありません。契約名だけでは付属クレジットや無料利用を判断できません。契約上限内かつ確認できたその月の付属クレジットの範囲で利用し、追加購入・追加入金は行いません。追加出費を防げる条件が確認できない場合は利用を開始しないでください。

## 配置前の確認

受付が配る設定の `workerTokenFile` の親ディレクトリがコンテナ内の `/state/private` と一致することを確認します。Workerはその親の `claude-api-key` を読みます。以下の `clientDir` は実際に使う絶対パスです。実際のclientDirのCompose設定とイメージで数値uidを確認します（起動済みコンテナがなければ確認できるまで中止）。

```bash
docker compose --project-directory "$clientDir" --env-file "$clientDir/.env" -f "$clientDir/compose.yaml" exec issue-desk-client id -u
docker compose --project-directory "$clientDir" --env-file "$clientDir/.env" -f "$clientDir/compose.yaml" exec issue-desk-client stat -c '%u %a %F' /state/private
```

`/state/private` の所有者も確認し、受付のtoken配置と整合することを確認してください。Dockerfileの `USER node` から数値uidや既存volumeの所有者を推測しません。不一致、アクセス不能、確認不能なら停止し、権限を自動変更して補わないでください。

Git管理外のホスト絶対パスにキーを先置きします。シンボリックリンクではない通常ファイル、モード0600、上で確認したコンテナnodeと同じ所有uid、4096バイト以内にしてください。値は画面に表示せず、履歴やログに残らない対話入力等で受け取り、改行を付けない `printf '%s'` 等でファイルへ書きます。シェルのトレースも無効にしてください。キーをコマンド引数や `.env` に書かないでください。現行形式は `sk-ant-` に続く20文字以上の英数字、アンダースコアまたはハイフンです。末尾LFまたはCRLFを1個許容しますが、複数改行、途中の改行、単独CRは拒否されます。

配置後、値を出さず `stat -c '%u %a %F %s' "$keyFile"` で所有uid・0600・通常ファイル・サイズを確認し、上記uidと照合します。形式は値を出さない検査で確認します。`keyFile` にそのホスト絶対パスを設定し、次のコマンドで結果だけを確認します。

```bash
KEY_FILE="$keyFile" python3 - <<'PY'
import os, re, stat
p = os.environ['KEY_FILE']
with open(p, 'rb') as f:
    s = os.fstat(f.fileno())
    data = f.read(4097)
if data.endswith(b'\r\n'): data = data[:-2]
elif data.endswith(b'\n'): data = data[:-1]
ok = stat.S_ISREG(s.st_mode) and stat.S_IMODE(s.st_mode) == 0o600 and s.st_size <= 4096 and re.fullmatch(rb'sk-ant-[A-Za-z0-9_-]{20,}', data) is not None
print('形式確認: OK' if ok else '形式確認: NG')
PY
```

## 反映と確認

引取りを停止し、進行中の実行と送信が完了したことを確認してから更新します。hostdのイメージ更新だけでは配置済みの `compose.yaml` と `update-linux.sh` は置換されません。変更前にclientDirの `.env` と旧2ファイルを安全なGit管理外の場所へ退避し、配布元 `deploy/compose.yaml` と `deploy/update-linux.sh` をそれぞれclientDir直下の同名ファイルへ反映します。既存の秘密、volume、ポート、その他の設定を維持してください。

clientDirの `.env` に `CLAUDE_API_KEY_FILE` として先置きしたホスト絶対パスを設定します（値そのものではありません）。未設定なら `/dev/null` が読み取り専用で取り付けられ、通常ファイルでないためキーなしになります。取付を後で外してstate volume上に空ファイルが残っても、形式に合わないためキーなしです。指定パスが存在しなければ `create_host_path: false` によりComposeのbind作成は失敗し、自動作成されません。以前の配布資産で対象パスに空ディレクトリが作られている場合は、空であることを確認して片付けてから通常ファイルを配置してください。

既存の `bash deploy/update-linux.sh <clientDir> <公開済みタグ>` で更新します。実際に使用する配布元スクリプトまたはclientDirに反映したスクリプトを指定してください。呼出元環境の同名変数は無視され、指定clientDirの `.env` が使われます。更新後、同じCompose指定で `exec issue-desk-client` を使い、`id -u` と `stat -c '%u %a %F %s' /state/private/claude-api-key` を確認し、nodeで読めることを `test -r /state/private/claude-api-key` で確認します。同じ単体ファイルの形式は上記のホスト側検査結果で確認し、workerTokenFileの親と取付先、通常ファイル・権限・サイズ・形式を照合します。取付成功、ready、契約認証成功はいずれもAPIキー自体の有効性の証明ではありません。実APIへの課金呼出しを確認に使わず、キーの有効性を確認できない場合はその旨を扱い、利用を再開しないでください。

失敗時は `bash deploy/update-linux.sh <clientDir> --restore-previous` を実行し、ログ、自己検査、受付readyを確認します。この操作は以前の環境を戻しますが、composeは復元しません。またスクリプトの退避 `.env` に新しい取付元設定が含まれる場合があります。必要なら変更前に別途退避した `.env` と旧2ファイルも戻し、整合性を確認してから再開します。
