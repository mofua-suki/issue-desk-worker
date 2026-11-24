package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"issue-desk-worker/internal/cause"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"issue-desk-worker/internal/execution"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

func maintainWorkspaces(ctx context.Context, eligible []string, refresh func(context.Context) error, requests func(context.Context, []string) error, cleanup func(context.Context, []string)) error {
	if err := refresh(ctx); err != nil {
		return err
	}
	// Cleanup only removes trees whose own records are finished, so a request
	// failure must not leave every finished tree in place on each cycle.
	err := requests(ctx, eligible)
	cleanup(ctx, eligible)
	return err
}

func credential(args []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("credential", flag.ContinueOnError)
	file := flags.String("token-file", "", "")
	origin := flags.String("origin", "", "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 || flags.Arg(0) != "get" {
		return nil
	}
	u, err := url.Parse(*origin)
	if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return cause.New("Git認証の接続先が不正です", err)
	}
	values := map[string]string{}
	scanner := bufio.NewScanner(io.LimitReader(input, 16384))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	if scanner.Err() != nil {
		return errors.New("Git認証の要求を読めません")
	}
	if values["protocol"] != u.Scheme || values["host"] != u.Host {
		return errors.New("Git認証の要求先が設定と一致しません")
	}
	token, err := gitea.ReadToken(*file)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "username=x-access-token\npassword=%s\n\n", token)
	return err
}
func isolationProbe(args []string) error {
	flags := flag.NewFlagSet("isolation-probe", flag.ContinueOnError)
	token := flags.String("token-file", "", "")
	sentinel := flags.String("sentinel", "", "")
	workdir := flags.String("workdir", "", "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	for _, file := range []string{*token, *sentinel} {
		handle, err := os.Open(file)
		if err == nil {
			handle.Close()
			return errors.New("保護ファイルを読めるため停止します")
		}
		if !errors.Is(err, os.ErrPermission) && !errors.Is(err, syscall.EPERM) {
			return errors.New("拒否された理由を確認できません")
		}
	}
	file, err := os.CreateTemp(*workdir, "isolation-write-")
	if err != nil {
		return cause.New("作業ツリーに書けません", err)
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Remove(name); err != nil {
		return err
	}
	fmt.Println("ISOLATION_OK")
	return nil
}

func isolationCheck(args []string) error {
	flags := flag.NewFlagSet("isolation-check", flag.ContinueOnError)
	root := flags.String("root", "", "")
	codex := flags.String("codex", "", "")
	token := flags.String("token-file", "", "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !filepath.IsAbs(*root) || !filepath.IsAbs(*codex) || !filepath.IsAbs(*token) {
		return errors.New("隔離検査にはroot・codex・token-fileの絶対パスが必要です")
	}
	store, err := journal.Open(*root)
	if err != nil {
		return err
	}
	defer store.Close()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	controller := execution.Executor{Config: execution.Configuration{Fleet: execution.Fleet{RuntimeDir: *root, Codex: *codex, WorkerTokenFile: *token}}, Executable: executable}
	return controller.VerifyIsolation(context.Background())
}

// Unknown or unavailable snapshots never clear an already configured verifier.
func managedVerifier(current, candidate string, state *intake.VerifierState) string {
	if state != nil {
		if !state.Known || !state.Available {
			return current
		}
		return candidate
	}
	if candidate != "" {
		return candidate
	}
	return current
}
