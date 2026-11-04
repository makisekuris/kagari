package weixin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLoginStatusesAndCredentialSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weixin.json")
	var qrCount, statusCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" || r.Header.Get("AuthorizationType") != "ilink_bot_token" {
				t.Errorf("unexpected QR request headers: %v", r.Header)
			}
			if r.Header.Get("iLink-App-Id") != "bot" || r.Header.Get("iLink-App-ClientVersion") != apiClientVersion {
				t.Errorf("missing app headers: %v", r.Header)
			}
			if raw := r.Header.Get("X-WECHAT-UIN"); raw == "" {
				t.Error("X-WECHAT-UIN is missing")
			} else if decoded, err := base64.StdEncoding.DecodeString(raw); err != nil {
				t.Errorf("invalid X-WECHAT-UIN: %v", err)
			} else if _, err := strconv.ParseUint(string(decoded), 10, 32); err != nil {
				t.Errorf("X-WECHAT-UIN is not a uint32: %q", decoded)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode QR body: %v", err)
			}
			if _, exists := body["base_info"]; exists {
				t.Error("QR request must not include base_info")
			}
			qrCount++
			_ = json.NewEncoder(w).Encode(map[string]string{"qrcode": "qr-secret-" + strconv.Itoa(qrCount), "qrcode_img_content": "https://qr.example/" + strconv.Itoa(qrCount)})
		case "/ilink/bot/get_qrcode_status":
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("AuthorizationType") != "" || r.Header.Get("X-WECHAT-UIN") != "" {
				t.Errorf("QR status request included bot auth: %v", r.Header)
			}
			if r.Header.Get("iLink-App-Id") != "bot" || r.Header.Get("iLink-App-ClientVersion") != apiClientVersion {
				t.Errorf("missing status app headers: %v", r.Header)
			}
			statusCount++
			if (statusCount == 4 || statusCount == 5) && r.URL.Query().Get("verify_code") != "1234" {
				t.Errorf("verify code was not carried to status poll: %s", r.URL.RawQuery)
			}
			if statusCount == 3 && r.URL.Query().Get("verify_code") != "" {
				t.Errorf("unexpected verification code before user input: %s", r.URL.RawQuery)
			}
			var body map[string]any
			switch statusCount {
			case 1:
				body = map[string]any{"status": "wait"}
			case 2:
				body = map[string]any{"status": "scaned"}
			case 3:
				body = map[string]any{"status": "need_verifycode"}
			case 4:
				body = map[string]any{"status": "scaned_but_redirect", "redirect_host": r.Host}
			case 5:
				body = map[string]any{"status": "expired"}
			case 6:
				if r.URL.Query().Get("qrcode") != "qr-secret-2" {
					t.Errorf("expired QR was not refreshed: %s", r.URL.RawQuery)
				}
				body = map[string]any{"status": "confirmed", "bot_token": "secret-token", "ilink_bot_id": "bot-account", "ilink_user_id": "owner", "baseurl": serverURL(r)}
			default:
				t.Errorf("unexpected status poll %d", statusCount)
				body = map[string]any{"status": "verify_code_blocked"}
			}
			_ = json.NewEncoder(w).Encode(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	// The test callback receives the exact image content for terminal QR rendering.
	var shown []string
	verifyCalls := 0
	checkCalls := 0
	credentials, err := loginAt(context.Background(), path, LoginCallbacks{
		ShowQR:     func(_ context.Context, content string) error { shown = append(shown, content); return nil },
		VerifyCode: func(context.Context) (string, error) { verifyCalls++; return "1234", nil },
		CheckBinding: func(_ context.Context, previous, next Credentials) error {
			checkCalls++
			if previous != (Credentials{}) || next.UserID != "owner" {
				t.Errorf("unexpected binding callback values: previous=%+v next.UserID=%q", previous, next.UserID)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("credential file existed before binding approval: %v", err)
			}
			return nil
		},
	}, server.URL, newHTTPClient(), true)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Token != "secret-token" || credentials.AccountID != "bot-account" || credentials.UserID != "owner" || credentials.BaseURL != server.URL {
		t.Fatalf("unexpected credentials: %+v", credentials)
	}
	if len(shown) != 2 || shown[0] != "https://qr.example/1" || shown[1] != "https://qr.example/2" {
		t.Fatalf("unexpected QR displays: %v", shown)
	}
	if verifyCalls != 1 || checkCalls != 1 || statusCount != 6 {
		t.Fatalf("unexpected callback/status counts: verify=%d binding=%d polls=%d", verifyCalls, checkCalls, statusCount)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("credential mode = %04o, want 0600", info.Mode().Perm())
	}
}

func TestLoginBindingRejectionKeepsOldCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weixin.json")
	old := Credentials{Token: "old-token", AccountID: "old-account", UserID: "old-owner", BaseURL: defaultAPIBaseURL}
	if err := SaveCredentials(path, old); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ilink/bot/get_bot_qrcode" {
			_ = json.NewEncoder(w).Encode(map[string]string{"qrcode": "q", "qrcode_img_content": "display"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "confirmed", "bot_token": "new-token", "ilink_bot_id": "new-account", "ilink_user_id": "new-owner", "baseurl": serverURL(r)})
	}))
	defer server.Close()
	callbacks := LoginCallbacks{ShowQR: func(context.Context, string) error { return nil }}
	_, err := loginAt(context.Background(), path, callbacks, server.URL, newHTTPClient(), true)
	if err == nil || !strings.Contains(err.Error(), "different user ID") {
		t.Fatalf("login should require explicit rebinding, got %v", err)
	}
	after, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if after != old {
		t.Fatalf("credentials changed before approval: %+v", after)
	}
	called := false
	callbacks.CheckBinding = func(_ context.Context, previous, next Credentials) error {
		called = true
		if previous != old || next.UserID != "new-owner" {
			t.Errorf("binding callback saw unexpected values: previous=%+v next=%+v", previous, next)
		}
		return errors.New("declined")
	}
	_, err = loginAt(context.Background(), path, callbacks, server.URL, newHTTPClient(), true)
	if err == nil || !called {
		t.Fatalf("binding rejection was not applied: called=%v err=%v", called, err)
	}
	after, err = LoadCredentials(path)
	if err != nil || after != old {
		t.Fatalf("credentials changed after rejected callback: %+v, %v", after, err)
	}
}

func TestLoginDoesNotOverwriteCredentialsChangedDuringScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weixin.json")
	old := Credentials{Token: "old-token", AccountID: "account", UserID: "owner", BaseURL: defaultAPIBaseURL}
	if err := SaveCredentials(path, old); err != nil {
		t.Fatal(err)
	}
	intermediate := Credentials{Token: "newer-token", AccountID: "account", UserID: "owner", BaseURL: defaultAPIBaseURL}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ilink/bot/get_bot_qrcode" {
			_ = json.NewEncoder(w).Encode(map[string]string{"qrcode": "q", "qrcode_img_content": "display"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "confirmed", "bot_token": "scan-token", "ilink_bot_id": "account", "ilink_user_id": "new-owner", "baseurl": serverURL(r)})
	}))
	defer server.Close()
	_, err := loginAt(context.Background(), path, LoginCallbacks{
		ShowQR: func(context.Context, string) error { return nil },
		CheckBinding: func(_ context.Context, previous, next Credentials) error {
			if previous != old || next.UserID != "new-owner" {
				t.Errorf("unexpected binding callback: previous=%+v next=%+v", previous, next)
			}
			return SaveCredentials(path, intermediate)
		},
	}, server.URL, newHTTPClient(), true)
	if err == nil || !strings.Contains(err.Error(), "changed during login") {
		t.Fatalf("login overwrote credentials changed during QR scan: %v", err)
	}
	after, err := LoadCredentials(path)
	if err != nil || after != intermediate {
		t.Fatalf("concurrent credential update was overwritten: %+v, %v", after, err)
	}
}

func serverURL(r *http.Request) string { return "http://" + r.Host }
