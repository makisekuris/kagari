package logging

import (
	"context"

	"go.uber.org/zap"
)

type loggerContextKey struct{}

func WithLogger(ctx context.Context, logger *zap.Logger) context.Context {
	return context.WithValue(ctx, loggerContextKey{}, logger)
}

func FromContext(ctx context.Context) *zap.Logger {
	if logger, ok := ctx.Value(loggerContextKey{}).(*zap.Logger); ok && logger != nil {
		return logger
	}
	return zap.NewNop()
}
