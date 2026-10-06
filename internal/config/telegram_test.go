package config

import (
	"os"
	"reflect"
	"testing"
)

func TestTelegramTargetsConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("KAGARI_TELEGRAM_TARGET_CHAT_IDS", "")
	if err := os.WriteFile("config.yaml", []byte("telegram:\n  target_chat_ids: [7, -100123]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// 空环境变量显式覆盖 YAML 列表，与其余配置键的优先级一致。
	cfg, err := Load("")
	if err != nil || len(cfg.Telegram.TargetChatIDs) != 0 {
		t.Fatalf("empty env override: %v, %v", cfg.Telegram.TargetChatIDs, err)
	}
	t.Setenv("KAGARI_TELEGRAM_TARGET_CHAT_IDS", "7,-100123")
	cfg, err = Load("")
	if err != nil || !reflect.DeepEqual(cfg.Telegram.ChatIDs(9), []int64{7, -100123}) {
		t.Fatalf("multi-target env: %v, %v", cfg.Telegram.TargetChatIDs, err)
	}
	for _, ids := range [][]int64{{0}, {7, 7}} {
		cfg.Telegram.TargetChatIDs = ids
		if err := cfg.Validate(false, false); err == nil {
			t.Fatalf("accepted invalid targets %v", ids)
		}
	}
	for _, tc := range []struct {
		config Telegram
		want   []int64
	}{
		{Telegram{}, []int64{9}},
		{Telegram{TargetChatIDs: []int64{7, -100456}}, []int64{7, -100456}},
	} {
		if got := tc.config.ChatIDs(9); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("ChatIDs = %v, want %v", got, tc.want)
		}
	}
}

func TestTelegramTargetsYAML(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, name := range []string{"KAGARI_TELEGRAM_TARGET_CHAT_IDS"} {
		old, present := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(name, old)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
	if err := os.WriteFile("config.yaml", []byte("telegram:\n  target_chat_ids: [7, -100123]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("")
	if err != nil || !reflect.DeepEqual(cfg.Telegram.ChatIDs(9), []int64{7, -100123}) {
		t.Fatalf("multi-target YAML: %v, %v", cfg.Telegram.TargetChatIDs, err)
	}
}
