package execution

import "issue-desk-worker/internal/gitwork"

// gitConfig is the Git configuration every workspace of this executor uses:
// the runtime directory as root and the worker token through the credential helper.
func (e *Executor) gitConfig() gitwork.Config {
	return gitwork.Config{Git: e.Config.Git, Root: e.Config.RuntimeDir, BaseURL: e.Config.GiteaURL, TokenFile: e.Config.WorkerTokenFile, Executable: e.Executable, User: e.Config.GitUser}
}
