package agent

import (
	"context"
	"log/slog"
	"time"
)

// drainBackgroundChat observes terminal outcomes after Manager has persisted
// them. A closed event channel means delivery ended, not that the task succeeded.
// Keep draining after an error so producers/cleanup cannot block on delivery.
func drainBackgroundChat(ctx context.Context, events <-chan ChatEvent, logger *slog.Logger, label, agentID string, timeout time.Duration) {
	var done bool
	var failure, code string
	for event := range events {
		if event.Type == "done" {
			done = true
		}
		if failure == "" && (event.Type == "error" || event.ErrorMessage != "") {
			failure, code = event.ErrorMessage, event.ErrorCode
			if failure == "" {
				failure = "backend reported an error without details"
			}
		}
	}

	switch {
	case ctx.Err() == context.DeadlineExceeded || failure == ErrMsgTimeout:
		logger.Warn(label+" timed out", "agent", agentID, "timeout", timeout, "err", failure, "errorCode", code)
	case ctx.Err() == context.Canceled || failure == ErrMsgCancelled || failure == "codex turn interrupted":
		logger.Info(label+" cancelled", "agent", agentID, "err", failure, "errorCode", code)
	case failure != "":
		logger.Warn(label+" failed", "agent", agentID, "err", failure, "errorCode", code)
	case !done:
		logger.Warn(label+" failed", "agent", agentID, "err", "event stream closed without a terminal result")
	default:
		logger.Info(label+" completed", "agent", agentID)
	}
}
