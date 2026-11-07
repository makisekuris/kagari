package config

import (
	"errors"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"kagari/internal/inputbudget"
)

const (
	maxLoggingSizeMiB       int64 = (1<<63 - 1) / (1 << 20)
	maxLoggingRetentionDays int64 = (1<<63 - 1) / (24 * int64(time.Hour))
)

// ValidatePersonal checks the single owner's service configuration.
func (c Config) ValidatePersonal() error {
	if c.Personal.Timezone == "" {
		return errors.New("personal.timezone must be a valid IANA time zone")
	}
	if c.Personal.Channel != "" && c.Personal.Channel != "telegram" && c.Personal.Channel != "weixin" {
		return errors.New("personal.channel must be telegram or weixin")
	}
	if err := c.Validate(true, c.Personal.Channel != "weixin"); err != nil {
		return err
	}
	if c.Personal.Channel != "weixin" && len(c.Telegram.AllowedUserIDs) != 1 {
		return errors.New("personal mode requires exactly one telegram.allowed_user_ids entry")
	}
	if c.Personal.TaskConcurrency < 1 || c.Personal.TaskBudget < time.Millisecond {
		return errors.New("personal.task_concurrency must be positive and personal.task_budget at least 1ms")
	}
	// Check conversion bounds before runtime turns MiB and days into bytes and time.Duration.
	if c.Logging.RotateMiB < 1 || c.Logging.RotateMiB > maxLoggingSizeMiB || c.Logging.MaxTotalMiB < 1 || c.Logging.MaxTotalMiB > maxLoggingSizeMiB {
		return errors.New("logging.rotate_mib and logging.max_total_mib must be positive MiB values that fit in int64 bytes")
	}
	if c.Logging.RetentionDays < 1 || int64(c.Logging.RetentionDays) > maxLoggingRetentionDays {
		return errors.New("logging.retention_days must be positive and fit in time.Duration")
	}
	if c.Logging.RotateMiB > c.Logging.MaxTotalMiB {
		return errors.New("logging.rotate_mib must not exceed logging.max_total_mib")
	}
	return nil
}

// Validate 先校验所有命令共享的边界，再按调用方需求检查凭证。
// 本地检查不要求凭证；启动服务时再检查模型和所选渠道。
func (c Config) Validate(modelRequired, telegramRequired bool) error {
	if c.Personal.Timezone == "Local" {
		return errors.New("personal.timezone must be a valid IANA time zone")
	}
	if _, err := time.LoadLocation(c.Personal.Timezone); c.Personal.Timezone != "" && err != nil {
		return errors.New("personal.timezone must be a valid IANA time zone")
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
	if c.Model.Timeout <= 0 || c.Reader.Timeout <= 0 || c.Agent.Timeout <= 0 {
		return errors.New("timeouts must be positive")
	}
	if c.Reader.MaxBytes < 1024 || c.Reader.MaxContentChars < 256 || c.Reader.MaxLinks < 1 || c.Model.MaxOutputTokens < 1 {
		return errors.New("reader and model limits must be positive")
	}
	if c.Agent.MaxSources < 1 || c.Agent.MaxSources > 30 || c.Agent.MaxIterations < 1 {
		return errors.New("invalid agent reading limits")
	}
	if modelRequired {
		if c.Model.ContextWindow <= 0 {
			return errors.New("model.context_window must be a positive input-plus-output token capacity")
		}
		if _, err := inputbudget.NewLimits(c.Model.ContextWindow, int64(c.Model.MaxOutputTokens)); err != nil {
			return err
		}
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
