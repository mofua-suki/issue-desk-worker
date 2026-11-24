package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"issue-desk-worker/internal/atomicfile"
	"issue-desk-worker/internal/cause"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"issue-desk-worker/internal/config"
	"issue-desk-worker/internal/execution"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/intake"
)

func tokenPlace(args []string, input io.Reader) error {
	flags := flag.NewFlagSet("token-place", flag.ContinueOnError)
	file := flags.String("config", "", "3項目の設定ファイル")
	phase := flags.String("phase", "check", "check・write・verify・existing")
	kind := flags.String("kind", "worker-token", "worker-token・gitea-token")
	host := flags.String("worker-id", "", "blueで照合したworkerId")
	if flags.Parse(args) != nil || *file == "" || flags.NArg() != 0 || (*phase != "check" && *phase != "write" && *phase != "verify" && *phase != "existing") || (*kind != "worker-token" && *kind != "gitea-token") {
		return errors.New("token-placeの設定・種類・段階を確認してください")
	}
	bootstrap, err := config.Load(*file)
	if err != nil {
		return err
	}
	if *host != "" && *host != bootstrap.WorkerID {
		return errors.New("stateのworkerIdがblueと一致しません")
	}
	serverToken, err := bootstrap.ReadToken()
	if err != nil {
		return err
	}
	api, err := intake.New(bootstrap, serverToken, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	snapshot, err := api.Config(ctx)
	if err != nil {
		return err
	}
	cfg, err := execution.DecodeConfig(snapshot, bootstrap.WorkerID)
	if err != nil {
		return err
	}
	expected := cfg.Assignments.WorkerAccounts[bootstrap.WorkerID]
	if *kind == "gitea-token" {
		expected = cfg.GitUser
	}
	if expected == "" {
		return errors.New("このworkerIdのGiteaアカウントが設定されていません")
	}
	private := filepath.Join(filepath.Dir(*file), "private")
	target := filepath.Join(private, *kind)
	var raw []byte
	if *phase == "existing" {
		stored, readErr := gitea.ReadToken(target)
		if readErr != nil {
			return readErr
		}
		raw = []byte(stored)
	} else {
		raw, err = io.ReadAll(io.LimitReader(input, 8193))
	}
	if err != nil || len(raw) > 8192 {
		return cause.New("トークンの入力を読めません", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsAny(token, " \n\r\t") {
		return errors.New("トークンの入力が不正です")
	}
	git, err := gitea.New(cfg.GiteaURL, token)
	if err != nil {
		return cause.New("Gitea接続先が不正です", err)
	}
	var user struct {
		Login string `json:"login"`
	}
	if git.Request(ctx, "GET", "/user", nil, &user) != nil {
		return errors.New("Giteaでトークンの所有者を検証できません")
	}
	if user.Login != expected {
		return errors.New("Giteaの所有者が設定アカウントと一致しません")
	}
	if *phase == "write" {
		if err := os.MkdirAll(private, 0700); err != nil {
			return cause.New("資格情報ディレクトリを作れません", err)
		}
		info, err := os.Lstat(private)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return cause.New("資格情報ディレクトリが不正です", err)
		}
		lockFD, err := syscall.Open(filepath.Join(private, "."+*kind+".lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return cause.New("資格情報の排他を取得できません", err)
		}
		defer syscall.Close(lockFD)
		if syscall.Flock(lockFD, syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			return errors.New("同じ資格情報を配置中です")
		}
		if err := os.Chmod(private, 0700); err != nil {
			return err
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(private, 1000, 1000); err != nil {
				return err
			}
		}
		err = atomicfile.Write(target, *kind+".", 0600, false, []byte(token), func(f *os.File) error {
			if os.Geteuid() == 0 {
				return f.Chown(1000, 1000)
			}
			return nil
		})
		if err != nil {
			var stage *atomicfile.Error
			if errors.As(err, &stage) {
				switch stage.Stage {
				case atomicfile.StageCreate:
					return errors.New("資格情報の置換ファイルを作れません")
				case atomicfile.StageWrite:
					return errors.New("資格情報を書き込めません")
				case atomicfile.StageRename:
					return errors.New("資格情報を原子的に置換できません")
				case atomicfile.StageSyncDir:
					return errors.New("資格情報ディレクトリを同期できません")
				case atomicfile.StageOpenDir:
					return stage.Err
				}
			}
			return err
		}
	}
	if *phase == "verify" || *phase == "existing" {
		info, err := os.Lstat(target)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return cause.New("保存された資格情報が0600の通常ファイルではありません", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		uid := os.Getuid()
		if uid == 0 {
			uid = 1000
		}
		if !ok || int(stat.Uid) != uid {
			return errors.New("保存された資格情報の所有者が違います")
		}
		stored, err := gitea.ReadToken(target)
		if err != nil || stored != token {
			return cause.New("保存された資格情報を検証できません", err)
		}
	}
	fmt.Println("Giteaアカウントの照合と配置段階を確認しました")
	return nil
}
