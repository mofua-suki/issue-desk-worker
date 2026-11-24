# 監視WorkerのCodex再ログイン

認証待ちを監視コンテナの再起動や置換に依存させないため、既存の認証保存先だけを使う一時コンテナで操作します。NASの受付用と監視用の保存先は異なります。受付用の場所を推測して指定せず、対象の監視コンテナから確認してください。認証ファイルやトークンの内容は表示・コピーしません。

1. 対象Workerの引取りを必要に応じて停止し、実行中の仕事と結果送信の完了を確認します。この作業で停止した引取りを記録し、元からの保留や別理由の停止はそのままにします。ログアウトが必要な場合も、まず以下の保存先・イメージ・所有uidを確認します。
2. `docker ps`で対象監視コンテナを特定し、その名前またはIDを`CONTAINER`に設定します。次で認証マウントの種別、保存元、宛先と、実際に稼働するローカルイメージIDを確認します。

   ```sh
   CONTAINER='確認した監視コンテナ名またはID'
   docker inspect --format '{{.Image}}' "$CONTAINER"
   docker inspect --format '{{range .Mounts}}{{if eq .Destination "/data/codex"}}{{.Type}} {{.Name}} {{.Source}} -> {{.Destination}}{{println}}{{end}}{{end}}' "$CONTAINER"
   docker exec "$CONTAINER" stat -c '%u:%g %n' /data/codex /data/codex/auth.json
   ```

   `stat`は内容を読まず所有uid:gidを表示します。ディレクトリと認証ファイルの所有者が一致しない、ファイルがない、または確認できない場合は止めます。イメージのユーザー名からuidを推測しません。`/data/codex`へのマウントが一意に確認でき、種別がbindまたはvolumeであることを確認します。bindは表示されたSourceが既存ディレクトリであることをホスト上で確認し、volumeは表示されたNameを`docker volume inspect 名前`で既存と確認します。保存先、ローカルイメージID、所有uidを確定できない場合は止めます。新しい資格情報の発行や所有権・権限の変更で補わないでください。
3. 確認した値を以下に設定します。`AUTH_SOURCE`はbindならSource、volumeならNameです。`--mount`は存在しないbind元を作成しません。volumeも事前に存在を確認し、不明な名前を指定しないでください。イメージIDはinspectの結果をそのまま使います。

   ```sh
   IMAGE='確認したローカルイメージID'
   AUTH_TYPE='bindまたはvolume'
   AUTH_SOURCE='確認した既存のSourceまたはName'
   AUTH_OWNER='確認したuid:gid'
   docker run --rm -it --pull=never --user "$AUTH_OWNER" \
     --env CODEX_HOME=/data/codex \
     --mount "type=$AUTH_TYPE,source=$AUTH_SOURCE,target=/data/codex" \
     --entrypoint /opt/agent-tools/bin/codex "$IMAGE" login --device-auth
   ```

   表示される案内に従って承認し、この一時コンテナを承認完了まで維持します。監視Worker内でログアウトとログインを連続実行しません。ログアウトが必要な場合は確認後、同じ保存先・イメージ・uidの一時コンテナで上記の`login --device-auth`を`logout`に替えて実行し、その後ログインします。Worker状態領域や他CLIの資格情報はマウントしません。
4. 承認後、同じ値のまま上記コマンド末尾の`login --device-auth`を`login status`に替えて実行し、成功を確認します。失敗したら止めます。次に対象Worker側で`docker exec "$CONTAINER" worker check --config /state/config.local.json`を実行し、契約認証を含む検査が成功して`executionReady=true`になることを確認します。確認できなければ止めます。すべて成功した後、この作業のために停止した引取りだけを戻します。
