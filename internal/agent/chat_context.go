package agent

import (
	"context"
	"sync"
)

type chatCompletion struct {
	done chan struct{}
	once sync.Once
}

type chatCompletionContextKey struct{}

// WithChatCompletion returns a context whose done channel is closed when the
// interactive Chat turn started with that context actually finishes. This is
// separate from ctx.Done: interactive turns intentionally survive a browser
// WebSocket disconnect.
func WithChatCompletion(ctx context.Context) (context.Context, <-chan struct{}) {
	c := &chatCompletion{done: make(chan struct{})}
	return context.WithValue(ctx, chatCompletionContextKey{}, c), c.done
}

func signalChatCompletion(ctx context.Context) {
	c, _ := ctx.Value(chatCompletionContextKey{}).(*chatCompletion)
	if c != nil {
		c.once.Do(func() { close(c.done) })
	}
}
