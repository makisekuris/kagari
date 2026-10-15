package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"kagari/internal/domain"
)

func enqueueDeliveryNotice(ctx context.Context, tx *sql.Tx, delivery domain.Delivery) error {
	if delivery.Status != "failed" && delivery.Status != "uncertain" {
		return nil
	}
	var kind string
	var payload []byte
	err := tx.QueryRowContext(ctx, `SELECT kind,payload FROM jobs WHERE id=?`, delivery.JobID).Scan(&kind, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var userID, chatID int64
	switch kind {
	case "analyze":
		var submission domain.Submission
		if json.Unmarshal(payload, &submission) != nil {
			return nil
		}
		userID, chatID = submission.UserID, submission.ChatID
	case "digest":
		var request domain.DigestRequest
		if json.Unmarshal(payload, &request) != nil {
			return nil
		}
		userID, chatID = request.UserID, request.UserID
	default:
		return nil
	}
	if userID <= 0 || chatID <= 0 {
		return nil
	}
	state := "投递失败"
	if delivery.Status == "uncertain" {
		state = "投递结果不确定"
	}
	text := fmt.Sprintf("任务 #%d %s（目标 %s/%s）。原任务结果已保留。使用 /status %d 查看，使用 /retry_delivery %d 重发。", delivery.JobID, state, delivery.Target.Channel, delivery.Target.Address, delivery.JobID, delivery.JobID)
	if delivery.Status == "uncertain" {
		text += "远端可能已发送，手动重发可能导致重复。"
	}
	raw, err := json.Marshal(domain.Command{UserID: userID, ChatID: chatID, Text: text})
	if err != nil {
		return err
	}
	key := fmt.Sprintf("delivery-notice:%d:%d", delivery.JobID, delivery.ID)
	target := domain.DeliveryTarget{Channel: "telegram", Address: strconv.FormatInt(chatID, 10)}
	_, _, err = enqueue(ctx, tx, "notice", key, raw, []domain.DeliveryTarget{target}, time.Now().UTC())
	return err
}
