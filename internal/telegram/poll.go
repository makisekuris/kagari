package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/go-telegram/bot/models"
	"go.uber.org/zap"
)

const (
	pollTimeout     = 20
	maxResponseSize = 8 << 20
	maxPollDelay    = 30 * time.Second
)

type pollFailure struct {
	code       int
	retryAfter time.Duration
}

func (e *pollFailure) Error() string { return "Telegram polling request failed" }

type updatesResponse struct {
	OK         bool              `json:"ok"`
	Result     []json.RawMessage `json:"result"`
	ErrorCode  int               `json:"error_code"`
	Parameters struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func (c *Client) Poll(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		offset, err := c.store.Offset(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errors.New("Telegram update state is unavailable")
		}
		updates, err := c.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var apiErr *pollFailure
			if errors.As(err, &apiErr) && (apiErr.code == http.StatusBadRequest || apiErr.code == http.StatusUnauthorized || apiErr.code == http.StatusForbidden) {
				return errors.New("Telegram polling request was rejected")
			}
			wait := delay
			if errors.As(err, &apiErr) && apiErr.retryAfter > 0 {
				wait = apiErr.retryAfter
			}
			c.log.Warn("Telegram polling failed; retrying", zap.Duration("retry_after", wait))
			if !sleep(ctx, wait) {
				return nil
			}
			if delay < maxPollDelay/2 {
				delay *= 2
			} else {
				delay = maxPollDelay
			}
			continue
		}
		delay = time.Second
		for _, raw := range updates {
			var update models.Update
			if err := json.Unmarshal(raw, &update); err != nil {
				var malformed struct {
					ID *int64 `json:"update_id"`
				}
				if json.Unmarshal(raw, &malformed) != nil || malformed.ID == nil || *malformed.ID <= 0 {
					return errors.New("Telegram returned an unreadable update")
				}
				// 即使内容无法读取，也按无来源的 update 原子推进 offset，避免重复阻塞轮询。
				if _, _, err := c.store.AcceptUpdate(ctx, *malformed.ID, "", "", nil, nil); err != nil {
					return errors.New("Telegram update could not be saved")
				}
				c.log.Warn("discarded unreadable Telegram update", zap.Int64("update_id", *malformed.ID))
				continue
			}
			if err := c.acceptUpdate(ctx, &update); err != nil {
				return err
			}
		}
	}
}

func (c *Client) getUpdates(ctx context.Context, offset int64) ([]json.RawMessage, error) {
	query := url.Values{
		"offset":  {fmt.Sprint(offset)},
		"limit":   {"100"},
		"timeout": {fmt.Sprint(pollTimeout)},
	}
	endpoint := c.baseURL + "/bot" + c.config.Token + "/getUpdates?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("could not create Telegram polling request")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("Telegram polling transport failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil || len(body) > maxResponseSize {
		return nil, errors.New("Telegram polling response could not be read")
	}
	var result updatesResponse
	if json.Unmarshal(body, &result) != nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, &pollFailure{code: resp.StatusCode}
		}
		return nil, errors.New("Telegram polling response was invalid")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !result.OK {
		code := result.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return nil, &pollFailure{code: code, retryAfter: retryAfter(result.Parameters.RetryAfter)}
	}
	return result.Result, nil
}

func retryAfter(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	const maxSeconds = int64((1<<63 - 1) / int64(time.Second))
	if int64(seconds) > maxSeconds {
		return time.Duration(1<<63 - 1)
	}
	return time.Duration(seconds) * time.Second
}

func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
