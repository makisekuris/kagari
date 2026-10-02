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
	for _, cidr := range c.Reader.AllowedNonPublicCIDRs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil || prefix.Addr().Zone() != "" || prefix.Addr().Is4In6() {
			return errors.New("reader.allowed_non_public_cidrs must contain valid IPv4 or IPv6 CIDRs")
		}
	}
	if err := validateBrowserFallback(c.Reader.BrowserFallback); err != nil {
		return err
	}
	if c.Reader.BrowserFallback.Enabled {
		return errors.New("reader.browser_fallback is not implemented; keep enabled=false")
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
	if c.Agent.MaxSources < 1 || c.Agent.MaxSources > 30 || c.Agent.MaxSupplemental < 0 || c.Agent.MaxDepth < 0 || c.Agent.MaxDepth > 5 || c.Agent.MaxIterations < 1 {
		return errors.New("invalid agent reading limits")
	}
	if len(c.Agent.Categories) == 0 || c.MaxAttempts < 1 {
		return errors.New("categories and max_attempts are required")
	}
	seen := map[string]bool{}
	for _, category := range c.Agent.Categories {
		if category == "" || seen[category] {
			return errors.New("categories must be nonempty and unique")
		}
		seen[category] = true
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
	if c.Weekly.InputLimit() < 1024 {
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

func validateBrowserFallback(cfg BrowserFallback) error {
	if cfg.Engine != "chromium" && cfg.Engine != "firefox" && cfg.Engine != "webkit" {
		return errors.New("reader.browser_fallback.engine must be chromium, firefox, or webkit")
	}
	switch cfg.Mode {
	case "launch":
		if cfg.Endpoint != "" {
			return errors.New("reader.browser_fallback.endpoint is only valid for connect or cdp mode")
		}
	case "connect", "cdp":
		if cfg.ExecutablePath != "" {
			return errors.New("reader.browser_fallback.executable_path is only valid for launch mode")
		}
		if cfg.Mode == "cdp" && cfg.Engine != "chromium" {
			return errors.New("reader.browser_fallback.cdp mode requires the chromium engine")
		}
		if err := validateBrowserEndpoint(cfg.Mode, cfg.Endpoint); err != nil {
			return err
		}
	default:
		return errors.New("reader.browser_fallback.mode must be launch, connect, or cdp")
	}
	return nil
}

func validateBrowserEndpoint(mode, endpoint string) error {
	invalid := errors.New("reader.browser_fallback.endpoint must be an absolute URL with a hostname and no userinfo or fragment")
	if endpoint == "" || endpoint != strings.TrimSpace(endpoint) || strings.Contains(endpoint, "#") {
		return invalid
	}
	u, err := url.Parse(endpoint)
	if err != nil || !u.IsAbs() || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return invalid
	}
	scheme := strings.ToLower(u.Scheme)
	if mode == "connect" && scheme != "ws" && scheme != "wss" {
		return errors.New("reader.browser_fallback.connect endpoint must use ws or wss")
	}
	if mode == "cdp" && scheme != "http" && scheme != "https" && scheme != "ws" && scheme != "wss" {
		return errors.New("reader.browser_fallback.cdp endpoint must use http, https, ws, or wss")
	}
	return nil
}
