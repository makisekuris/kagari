package personal

import (
	"context"
	"time"
)

func (r *Runtime) schedule(ctx context.Context) (bool, error) {
	o, err := r.Store.DispatchDueSchedule(ctx, time.Now().UTC())
	return o != nil, err
}
