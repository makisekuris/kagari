package service

import (
	"context"
	"time"
)

func (r *Runtime) schedule(ctx context.Context) (bool, error) {
	now := time.Now().UTC()
	reminded, err := r.Store.DispatchDueReminder(ctx, now)
	if err != nil {
		return false, err
	}
	o, err := r.Store.DispatchDueSchedule(ctx, now)
	return reminded || o != nil, err
}
