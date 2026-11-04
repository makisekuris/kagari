package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
	"kagari/internal/app"
	"kagari/internal/config"
	"kagari/internal/personal"
	"kagari/internal/weixin"
)

func credentialPath(cfg config.Config) string { return cfg.Storage.Path + ".channels/weixin.json" }

func runChannelCommand(ctx context.Context, cfg config.Config, args []string, in io.Reader, out io.Writer) error {
	if len(args) < 2 {
		return errors.New("usage: kagari weixin login|rebind [-qr FILE.png] | channel status | attachments list|delete ID")
	}
	if args[0] == "channel" {
		if len(args) != 2 || args[1] != "status" {
			return errors.New("usage: kagari channel status")
		}
		status := map[string]any{"channel": cfg.Personal.Channel, "database": cfg.Storage.Path}
		if cfg.Personal.Channel == "weixin" {
			credentials, err := weixin.LoadCredentials(credentialPath(cfg))
			if err != nil {
				status["credentials"] = "unavailable"
				status["action"] = "kagari weixin login"
			} else {
				status["credentials"] = "saved"
				status["account"] = credentials.AccountID
				status["user"] = credentials.UserID
			}
		}
		status["note"] = "Saved configuration is not proof of a live connection; inspect service logs for transport errors."
		return json.NewEncoder(out).Encode(status)
	}
	if args[0] == "weixin" && args[1] == "rebind" {
		unlock, err := app.Lock(cfg.Storage.Path)
		if err != nil {
			return fmt.Errorf("换绑前请停止使用该数据库的服务: %w", err)
		}
		defer unlock()
	}
	store, err := personal.NewStore(cfg.Storage.Path, cfg.Personal.TaskBudget)
	if err != nil {
		return err
	}
	defer store.Close()
	if args[0] == "attachments" {
		switch {
		case len(args) == 2 && args[1] == "list":
			files, err := store.Attachments(ctx)
			if err != nil {
				return err
			}
			return json.NewEncoder(out).Encode(files)
		case len(args) == 3 && args[1] == "delete":
			if err := store.DeleteAttachment(ctx, args[2]); err != nil {
				return err
			}
			_, err := fmt.Fprintln(out, "附件原文件已删除；历史、分析结果和已发出的文件仍保留。")
			return err
		default:
			return errors.New("usage: kagari attachments list | delete ID")
		}
	}
	if args[0] != "weixin" || (args[1] != "login" && args[1] != "rebind") {
		return errors.New("usage: kagari weixin login|rebind [-qr FILE.png]")
	}
	flags := flag.NewFlagSet("weixin "+args[1], flag.ContinueOnError)
	flags.SetOutput(out)
	qrPath := flags.String("qr", "", "save the login QR code as a PNG")
	if err := flags.Parse(args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected login argument")
	}
	unlockLogin, err := app.Lock(credentialPath(cfg))
	if err != nil {
		return fmt.Errorf("另一个微信登录流程正在运行: %w", err)
	}
	defer unlockLogin()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var qrFile *os.File
	if *qrPath != "" {
		if err := os.MkdirAll(filepath.Dir(*qrPath), 0700); err != nil {
			return err
		}
		qrFile, err = os.OpenFile(*qrPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		defer qrFile.Close()
	}
	reader := bufio.NewReader(in)
	var previous weixin.Credentials
	credentials, err := weixin.Login(ctx, credentialPath(cfg), weixin.LoginCallbacks{
		ShowQR: func(ctx context.Context, url string) error {
			return showLoginQR(out, url, qrFile)
		},
		VerifyCode: func(ctx context.Context) (string, error) {
			fmt.Fprint(out, "请输入微信显示的验证码：")
			type answer struct {
				text string
				err  error
			}
			result := make(chan answer, 1)
			go func() { line, err := reader.ReadString('\n'); result <- answer{line, err} }()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case a := <-result:
				return strings.TrimSpace(a.text), a.err
			}
		},
		CheckBinding: func(ctx context.Context, old, next weixin.Credentials) error {
			previous = old
			return checkWeixinBinding(ctx, store, old, next, args[1] == "rebind")
		},
	})
	if err != nil {
		return err
	}
	if err := commitWeixinBinding(ctx, store, credentialPath(cfg), previous, credentials, args[1] == "rebind"); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "微信账号 %s 已授权，凭证已保存。同一账号重新登录后，运行中的微信渠道会自动重载。\n", credentials.UserID)
	return err
}

func showLoginQR(out io.Writer, content string, pngFile *os.File) error {
	qr, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "%s\n用微信扫码并确认。备用链接：%s\n", qr.ToSmallString(false), content); err != nil {
		return err
	}
	if pngFile == nil {
		return nil
	}
	data, err := qr.PNG(512)
	if err != nil {
		return err
	}
	if err := pngFile.Truncate(0); err != nil {
		return err
	}
	if _, err := pngFile.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := pngFile.Write(data); err != nil {
		return err
	}
	return pngFile.Sync()
}

func checkWeixinBinding(ctx context.Context, store *personal.Store, old, next weixin.Credentials, rebind bool) error {
	if old.UserID != "" && old.UserID != next.UserID && !rebind {
		return errors.New("微信扫码用户与已保存凭证不同；请使用 weixin rebind 明确换绑")
	}
	return store.CheckChannelBinding(ctx, "weixin", next.AccountID, next.UserID, rebind)
}

func commitWeixinBinding(ctx context.Context, store *personal.Store, path string, old, next weixin.Credentials, rebind bool) error {
	if err := store.BindChannel(ctx, "weixin", next.AccountID, next.UserID, rebind); err != nil {
		var restoreErr error
		if old.Token != "" {
			restoreErr = weixin.SaveCredentials(path, old)
		} else {
			restoreErr = os.Remove(path)
		}
		return errors.Join(fmt.Errorf("save Weixin binding: %w", err), restoreErr)
	}
	return nil
}
