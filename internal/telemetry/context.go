package telemetry

import "context"

type contextKey struct{}

var runIDKey = contextKey{}

// WithRunID returns a new context annotated with the given runID.
func WithRunID(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, runIDKey, runID)
}

// RunIDFromContext retrieves the runID from context, or returns an empty string if unset.
func RunIDFromContext(ctx context.Context) string {
	if val, ok := ctx.Value(runIDKey).(string); ok {
		return val
	}
	return ""
}
