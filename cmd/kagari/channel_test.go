package main

import (
	"bytes"
	"context"
	"image/png"
	"io"
	"kagari/internal/weixin"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kagari/internal/personal"
)

func acceptTestInput(ctx context.Context, s *personal.Store, text string) (int64, error) {
	if err := s.BindChannel(ctx, "telegram", "bot", "owner", false); err != nil {
		return 0, err
	}
	ids, err := s.AcceptBatch(ctx, "telegram", "bot", "1", []personal.Input{{Channel: "telegram", Account: "bot", Sender: "owner", MessageID: "10", Text: text}})
	if err != nil {
		return 0, err
	}
	return ids[0], nil
}

func TestLoginQRRefresh(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "qr.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out bytes.Buffer
	for _, content := range []string{"https://example.org/long-login-link", "https://example.org/qr"} {
		if err := showLoginQR(&out, content, f); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if _, err := png.Decode(f); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Contains(out.Bytes(), []byte("用微信扫码")) {
		t.Fatal("missing instructions")
	}
}

func TestWeixinLoginCannotReplaceStoredOwner(t *testing.T) {
	s, err := personal.NewStore(filepath.Join(t.TempDir(), "personal.db"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old := weixin.Credentials{AccountID: "old-bot", UserID: "old-owner"}
	next := weixin.Credentials{AccountID: "new-bot", UserID: "new-owner"}
	if err := checkWeixinBinding(context.Background(), s, old, next, false); err == nil {
		t.Fatal("replaced stored owner without rebind")
	}
	if err := checkWeixinBinding(context.Background(), s, old, next, true); err != nil {
		t.Fatal(err)
	}
	if err := checkWeixinBinding(context.Background(), s, next, next, false); err != nil {
		t.Fatal(err)
	}
}

func TestWeixinBindingFailureRestoresCredentials(t *testing.T) {
	s, err := personal.NewStore(filepath.Join(t.TempDir(), "personal.db"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	old := weixin.Credentials{Token: "old-token", AccountID: "old-bot", UserID: "old-owner", BaseURL: "https://ilinkai.weixin.qq.com"}
	next := weixin.Credentials{Token: "new-token", AccountID: "new-bot", UserID: "new-owner", BaseURL: old.BaseURL}
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := weixin.SaveCredentials(path, next); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := commitWeixinBinding(context.Background(), s, path, old, next, true); err == nil {
		t.Fatal("closed database accepted binding")
	}
	restored, err := weixin.LoadCredentials(path)
	if err != nil || restored != old {
		t.Fatalf("previous credentials not restored: %+v %v", restored, err)
	}
}
