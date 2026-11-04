package weixin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// LoginCallbacks keeps terminal interaction and account binding policy in the CLI.
type LoginCallbacks struct {
	ShowQR       func(context.Context, string) error
	VerifyCode   func(context.Context) (string, error)
	CheckBinding func(context.Context, Credentials, Credentials) error
}

type qrStartResponse struct {
	QRCode       string `json:"qrcode"`
	ImageContent string `json:"qrcode_img_content"`
}

type qrStatusResponse struct {
	Status       string `json:"status"`
	Token        string `json:"bot_token"`
	AccountID    string `json:"ilink_bot_id"`
	BaseURL      string `json:"baseurl"`
	UserID       string `json:"ilink_user_id"`
	RedirectHost string `json:"redirect_host"`
}

func Login(ctx context.Context, credentialPath string, callbacks LoginCallbacks) (Credentials, error) {
	return loginAt(ctx, credentialPath, callbacks, defaultAPIBaseURL, newHTTPClient(), false)
}

// loginAt is the package-private HTTP seam used by httptest. Production Login always uses Tencent's fixed host.
func loginAt(ctx context.Context, credentialPath string, callbacks LoginCallbacks, apiBaseURL string, httpClient *http.Client, allowTestURL bool) (Credentials, error) {
	if callbacks.ShowQR == nil {
		return Credentials{}, errors.New("Weixin login requires a QR display callback")
	}
	if err := validateAPIBaseURL(apiBaseURL, allowTestURL); err != nil {
		return Credentials{}, err
	}

	previous, err := LoadCredentials(credentialPath)
	hadPrevious := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Credentials{}, fmt.Errorf("load existing Weixin binding: %w", err)
	}

	qr, err := fetchQRCode(ctx, httpClient, apiBaseURL, allowTestURL)
	if err != nil {
		return Credentials{}, err
	}
	if err := callbacks.ShowQR(ctx, qr.ImageContent); err != nil {
		return Credentials{}, fmt.Errorf("display Weixin QR code: %w", err)
	}

	pollBaseURL := apiBaseURL
	verifyCode := ""
	refreshes := 0
	for {
		if err := ctx.Err(); err != nil {
			return Credentials{}, err
		}
		status, err := pollQRStatus(ctx, httpClient, pollBaseURL, qr.QRCode, verifyCode, allowTestURL)
		if err != nil {
			return Credentials{}, err
		}
		switch status.Status {
		case "wait", "scaned":
			if status.Status == "scaned" {
				verifyCode = ""
			}
			if err := sleepContext(ctx, time.Second); err != nil {
				return Credentials{}, err
			}
		case "need_verifycode":
			if callbacks.VerifyCode == nil {
				return Credentials{}, errors.New("Weixin requires a verification code, but no input callback was provided")
			}
			verifyCode, err = callbacks.VerifyCode(ctx)
			if err != nil {
				return Credentials{}, fmt.Errorf("read Weixin verification code: %w", err)
			}
			if strings.TrimSpace(verifyCode) == "" {
				return Credentials{}, errors.New("Weixin verification code is empty")
			}
		case "expired":
			if refreshes >= 2 {
				return Credentials{}, errors.New("Weixin QR code expired repeatedly; start login again")
			}
			refreshes++
			qr, err = fetchQRCode(ctx, httpClient, apiBaseURL, allowTestURL)
			if err != nil {
				return Credentials{}, err
			}
			pollBaseURL = apiBaseURL
			verifyCode = ""
			if err := callbacks.ShowQR(ctx, qr.ImageContent); err != nil {
				return Credentials{}, fmt.Errorf("display refreshed Weixin QR code: %w", err)
			}
		case "scaned_but_redirect":
			if status.RedirectHost != "" {
				scheme := "https://"
				if allowTestURL && strings.HasPrefix(apiBaseURL, "http://") {
					scheme = "http://"
				}
				pollBaseURL = scheme + strings.TrimRight(status.RedirectHost, "/")
				if err := validateAPIBaseURL(pollBaseURL, allowTestURL); err != nil {
					return Credentials{}, fmt.Errorf("reject Weixin login redirect: %w", err)
				}
			}
		case "verify_code_blocked":
			return Credentials{}, errors.New("Weixin verification code was blocked; start login again")
		case "binded_redirect":
			return Credentials{}, errors.New("Weixin reports this bot is already bound; existing credentials were kept")
		case "confirmed":
			credentials := Credentials{Token: status.Token, AccountID: status.AccountID, UserID: status.UserID, BaseURL: status.BaseURL}
			if err := validateCredentials(credentials, allowTestURL); err != nil {
				return Credentials{}, fmt.Errorf("incomplete Weixin login response: %w", err)
			}
			if callbacks.CheckBinding != nil {
				if err := callbacks.CheckBinding(ctx, previous, credentials); err != nil {
					return Credentials{}, fmt.Errorf("Weixin binding was not approved: %w", err)
				}
			} else if hadPrevious && previous.UserID != credentials.UserID {
				return Credentials{}, errors.New("Weixin login returned a different user ID; explicit rebinding is required")
			}
			if err := saveCredentialsAfterBinding(credentialPath, previous, hadPrevious, credentials, allowTestURL); err != nil {
				return Credentials{}, err
			}
			return credentials, nil
		default:
			return Credentials{}, fmt.Errorf("unknown Weixin QR login status %q", status.Status)
		}
	}
}

func fetchQRCode(ctx context.Context, httpClient *http.Client, baseURL string, allowTestURL bool) (qrStartResponse, error) {
	if err := validateAPIBaseURL(baseURL, allowTestURL); err != nil {
		return qrStartResponse{}, err
	}
	u := strings.TrimRight(baseURL, "/") + "/ilink/bot/get_bot_qrcode?bot_type=3"
	headers, err := apiHeaders("", false)
	if err != nil {
		return qrStartResponse{}, err
	}
	data, _, err := request(ctx, httpClient, http.MethodPost, u, headers, []byte(`{"local_token_list":[]}`), apiTimeout)
	if err != nil {
		return qrStartResponse{}, fmt.Errorf("request Weixin QR code: %w", err)
	}
	var response qrStartResponse
	if err := parseJSON(data, &response); err != nil {
		return qrStartResponse{}, err
	}
	if response.QRCode == "" || response.ImageContent == "" {
		return qrStartResponse{}, errors.New("Weixin QR response is missing its code or display content")
	}
	return response, nil
}

func pollQRStatus(ctx context.Context, httpClient *http.Client, baseURL, qrcode, verifyCode string, allowTestURL bool) (qrStatusResponse, error) {
	u, err := makeQRURL(baseURL, qrcode, verifyCode, allowTestURL)
	if err != nil {
		return qrStatusResponse{}, err
	}
	if allowTestURL {
		if err := validateAPIBaseURL(baseURL, true); err != nil {
			return qrStatusResponse{}, err
		}
	}
	data, _, err := request(ctx, httpClient, http.MethodGet, u, qrStatusHeaders(), nil, qrPollTimeout)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return qrStatusResponse{Status: "wait"}, nil
		}
		return qrStatusResponse{}, fmt.Errorf("poll Weixin QR status: %w", err)
	}
	var response qrStatusResponse
	if err := parseJSON(data, &response); err != nil {
		return qrStatusResponse{}, err
	}
	return response, nil
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
