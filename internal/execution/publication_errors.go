package execution

import "errors"

var (
	errPRCreationUncertain = errors.New("PR作成の結果を照合中です。未知の投稿を自動再送しません")
	errPullMismatch        = errors.New("公開PRのリポジトリ・枝・既定ブランチ・headが一致しません")
	errChangesRequested    = errors.New("変更要求のレビューが残っています")
	errReviewTreeChanged   = errors.New("レビュー後に作業ツリーが変わりました")
	errMergeUnverified     = errors.New("マージ後のコミットを確認できません")
)

var publicationErrors = []error{
	errPRCreationUncertain,
	errPullMismatch,
	errChangesRequested,
	errReviewTreeChanged,
	errMergeUnverified,
}
