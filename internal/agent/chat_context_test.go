package agent

import (
	"context"
	"testing"
)

func TestChatCompletionSignalsOnce(t *testing.T) {
	ctx, done := WithChatCompletion(context.Background())
	signalChatCompletion(ctx)
	signalChatCompletion(ctx)
	select {
	case <-done:
	default:
		t.Fatal("chat completion was not signaled")
	}
}
