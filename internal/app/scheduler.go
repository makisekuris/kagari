package app

import (
	"context"
	"errors"
	"time"

	"kagari/internal/digest"
	"kagari/internal/telegram"
)

func schedule(ctx context.Context, w *Worker, now time.Time) error {
	const key = "weekly_cursor"
	if !w.Config.Weekly.Enabled {
		return w.Store.SetMeta(ctx, key, "")
	}
	value, err := w.Store.Meta(ctx, key)
	if err != nil {
		return err
	}
	if value == "" {
		// 启用时刻是 catch-up 的起点，首次启动不回填启用前的周报。
		return w.Store.SetMeta(ctx, key, now.UTC().Format(time.RFC3339Nano))
	}
	cursor, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return errors.New("invalid stored weekly cursor")
	}
	requests, err := digest.Due(now, cursor, w.Config.Weekly)
	if err != nil {
		return err
	}
	for _, request := range requests {
		for _, userID := range w.Config.Telegram.AllowedUserIDs {
			request.UserID = userID
			chatIDs := w.Config.Telegram.ChatIDs(userID)
			if _, _, err := w.EnqueueDigest(ctx, request, chatIDs[0], telegram.Targets(chatIDs)...); err != nil {
				return err
			}
		}
	}
	// 所有周期先持久入队再推进游标；若中途重启，周期幂等键保证补跑不会重复创建任务。
	if now.After(cursor) {
		return w.Store.SetMeta(ctx, key, now.UTC().Format(time.RFC3339Nano))
	}
	return nil
}
