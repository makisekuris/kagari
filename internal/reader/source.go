package reader

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"kagari/internal/domain"
)

const (
	statusOK         = "ok"
	statusIncomplete = "incomplete"
	statusRestricted = "restricted"
	statusFailed     = "failed"
)

func newSource(requested, final string) domain.Source {
	s := domain.Source{
		RequestedURL: requested,
		URL:          final,
		Kind:         "webpage",
		ReadMethod:   "readability",
		FetchedAt:    time.Now().UTC(),
	}
	s.ID = sourceID(final)
	return s
}

// finishError 在返回错误时仍保留 Source。restricted 表示访问被拒绝或目标内容无法验证，
// failed 表示输入或请求等技术失败；incomplete 表示提取不完整，也可能没有正文。
// 上层只有在 Content 非空且 Truncated 时才可引用片段，并须披露截断与 Reason。
func finishError(source domain.Source, status, reason string, err error) (domain.Source, error) {
	source.Status = status
	source.Reason = reason
	return source, err
}

func failedSource(requested, final, status, reason string, err error) (domain.Source, error) {
	return finishError(newSource(requested, final), status, reason, err)
}

func sourceID(normalizedURL string) string {
	sum := sha256.Sum256([]byte(normalizedURL))
	return hex.EncodeToString(sum[:])
}

func safeErrorReason(err error) string {
	if errors.Is(err, errUnsafeTarget) {
		return errUnsafeTarget.Error()
	}
	if errors.Is(err, errUnverifiedXStatus) {
		return errUnverifiedXStatus.Error()
	}
	return "could not fetch page"
}
