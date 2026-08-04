package agent

import (
	"context"
	"log/slog"
)

// logRPCSpan logs "executing RPC" and returns a function to defer for "RPC execution complete"
// (runs when the handler returns, after any later-registered defers).
func logRPCSpan(ctx context.Context, rpc string) func() {
	if ctx == nil {
		ctx = context.Background()
	}
	slog.InfoContext(ctx, "executing RPC", "rpc", rpc)
	return func() {
		slog.InfoContext(ctx, "RPC execution complete", "rpc", rpc)
	}
}
