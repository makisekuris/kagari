package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Validate 先校验所有命令共享的边界，再按调用方需求检查凭证。
// read 和归档回放不要求凭证；生成非空周报及 analyze/run 再启用模型检查。
func (c Config) Validate(modelRequired, telegramRequired bool) error {
	seenChats := map[int64]bool{}
	for _, id := range c.Telegram.TargetChatIDs {
		if id == 0 || seenChats[id] {
			return errors.New("telegram.target_chat_ids must contain nonzero, unique chat IDs")
		}
		seenChats[id] = true
	}
	for _, cidr := range c.Reader.AllowedNonPublicCIDRs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil || prefix.Addr().Zone() != "" || prefix.Addr().Is4In6() {
			return errors.New("reader.allowed_non_public_cidrs must contain valid IPv4 or IPv6 CIDRs")
		}
	}
	if c.Browser.Enabled && strings.TrimSpace(c.Browser.Command) == "" {
		return errors.New("browser.command is required when browser.enabled=true")
	}
	for _, arg := range c.Browser.Args {
		for _, flag := range []string{"--config", "--proxy", "--cdp", "--endpoint", "--extension", "--allow-unrestricted-file-access", "--browser", "--user-data-dir", "--storage-state"} {
			if arg == flag || strings.HasPrefix(arg, flag+"=") || strings.HasPrefix(arg, "--cdp-") || strings.HasPrefix(arg, "--proxy-") {
				return errors.New("browser.args must not override the isolated browser or network policy")
			}
		}
	}
	if strings.TrimSpace(c.Storage.Path) == "" {
		return errors.New("storage.path is required")
	}
	if strings.HasPrefix(c.Storage.Path, "file:") {
		return errors.New("storage.path must be a filesystem path, not a SQLite URI")
	}
	if c.Model.Timeout <= 0 || c.Reader.Timeout <= 0 || c.Agent.Timeout <= 0 || c.Reader.CacheTTL < 0 {
		return errors.New("timeouts must be positive and cache_ttl nonnegative")
	}
	if c.Reader.MaxBytes < 1024 || c.Reader.MaxContentChars < 256 || c.Reader.MaxLinks < 1 || c.Model.MaxOutputTokens < 1 {
		return errors.New("reader and model limits must be positive")
	}
	if c.Agent.MaxSources < 1 || c.Agent.MaxSources > 30 || c.Agent.MaxIterations < 1 {
		return errors.New("invalid agent reading limits")
	}
	if c.MaxAttempts < 1 {
		return errors.New("max_attempts must be positive")
	}
	if _, err := time.LoadLocation(c.Weekly.Timezone); err != nil {
		return fmt.Errorf("weekly.timezone: %w", err)
	}
	if c.Weekly.Weekday < 0 || c.Weekly.Weekday > 6 {
		return errors.New("weekly.weekday must be 0..6 (Sunday..Saturday)")
	}
	if _, err := time.Parse("15:04", c.Weekly.Time); err != nil {
		return errors.New("weekly.time must be HH:MM")
	}
	if c.Weekly.MaxInputChars < 1024 {
		return errors.New("weekly.max_input_chars must be at least 1024")
	}
	if modelRequired {
		if c.Model.Name == "" || c.Model.APIKey == "" || c.Model.BaseURL == "" {
			return errors.New("model.base_url, model.name and model.api_key are required")
		}
		u, err := url.Parse(c.Model.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("model.base_url must be an HTTP(S) base URL without credentials, query or fragment")
		}
	}
	if telegramRequired {
		if c.Telegram.Token == "" || len(c.Telegram.AllowedUserIDs) == 0 {
			return errors.New("telegram.token and telegram.allowed_user_ids are required")
		}
		for _, id := range c.Telegram.AllowedUserIDs {
			if id <= 0 {
				return errors.New("telegram.allowed_user_ids must contain positive user IDs")
			}
		}
	}
	return nil
}
