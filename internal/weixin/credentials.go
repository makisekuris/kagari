package weixin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	defaultAPIBaseURL = "https://ilinkai.weixin.qq.com"
	defaultCDNBaseURL = "https://novac2c.cdn.weixin.qq.com/c2c"
)

var stateWriteMu sync.Mutex

// Credentials are returned by the QR login flow and stored with mode 0600.
type Credentials struct {
	Token     string `json:"token"`
	AccountID string `json:"account_id"`
	UserID    string `json:"user_id"`
	BaseURL   string `json:"base_url"`
}

func LoadCredentials(path string) (Credentials, error) {
	f, err := os.Open(path)
	if err != nil {
		return Credentials{}, err
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil {
		return Credentials{}, fmt.Errorf("read Weixin credentials: %w", err)
	}
	if len(data) > 64<<10 {
		return Credentials{}, errors.New("Weixin credential file exceeds the size limit")
	}
	var credentials Credentials
	if err := json.Unmarshal(data, &credentials); err != nil {
		return Credentials{}, fmt.Errorf("read Weixin credentials: %w", err)
	}
	if err := validateCredentials(credentials, false); err != nil {
		return Credentials{}, err
	}
	return credentials, nil
}

func SaveCredentials(path string, credentials Credentials) error {
	if err := validateCredentials(credentials, false); err != nil {
		return err
	}
	data, err := json.MarshalIndent(credentials, "", "  ")
	if err != nil {
		return err
	}
	stateWriteMu.Lock()
	defer stateWriteMu.Unlock()
	return atomicPrivateWrite(path, data)
}

func saveCredentialsAfterBinding(path string, expected Credentials, hadExpected bool, credentials Credentials, allowTestURL bool) error {
	if err := validateCredentials(credentials, allowTestURL); err != nil {
		return err
	}
	data, err := json.MarshalIndent(credentials, "", "  ")
	if err != nil {
		return err
	}
	stateWriteMu.Lock()
	defer stateWriteMu.Unlock()
	current, err := LoadCredentials(path)
	if hadExpected {
		if err != nil || current != expected {
			return errors.New("Weixin credentials changed during login; binding was not saved")
		}
	} else if err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("Weixin credentials appeared during login; binding was not saved")
	}
	return atomicPrivateWrite(path, data)
}

func validateCredentials(c Credentials, allowTestURL bool) error {
	if strings.TrimSpace(c.Token) == "" || strings.TrimSpace(c.AccountID) == "" || strings.TrimSpace(c.UserID) == "" {
		return errors.New("Weixin credentials require token, account ID, and user ID")
	}
	if err := validateAPIBaseURL(c.BaseURL, allowTestURL); err != nil {
		return fmt.Errorf("invalid Weixin API base URL: %w", err)
	}
	return nil
}

func validateAPIBaseURL(raw string, allowTestURL bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("must be an HTTPS API origin")
	}
	if allowTestURL && u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1") {
		return nil
	}
	if u.Scheme != "https" || !isOfficialAPIHost(u.Hostname()) || u.Port() != "" {
		return errors.New("must use an official Tencent HTTPS host")
	}
	return nil
}

func isOfficialAPIHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "ilinkai.weixin.qq.com" || strings.HasSuffix(host, ".ilinkai.weixin.qq.com")
}

func atomicPrivateWrite(path string, data []byte) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("credential path is empty")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".weixin-credential-*")
	if err != nil {
		return fmt.Errorf("create temporary credential file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("secure temporary credential file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write credential file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync credential file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close credential file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace credential file: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		defer d.Close()
		if err := d.Sync(); err != nil {
			return fmt.Errorf("sync credential directory: %w", err)
		}
	}
	return nil
}
