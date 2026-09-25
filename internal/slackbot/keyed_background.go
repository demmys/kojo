package slackbot

import (
	"context"
	"fmt"
	"strings"

	"github.com/loppo-llc/kojo/internal/agent"
)

// keyedBackgroundRegistrar is the optional ChatManager capability that routes
// unsolicited turns of a lingering keyed claude session (run_in_background
// completion notifications) back to this bot.
type keyedBackgroundRegistrar interface {
	RegisterKeyedBackgroundHandler(agentID string, h agent.KeyedBackgroundHandler) func()
}

// backgroundPendingNote is appended to a final reply whose session keeps
// lingering for still-running background tasks.
func backgroundPendingNote(n int) string {
	return fmt.Sprintf("_バックグラウンド処理 %d件 実行中。完了したらこのスレッドで続きを投稿します_", n)
}

// registerKeyedBackground installs the bot as the agent's keyed background
// handler when the manager supports it. The returned func unregisters.
func (b *Bot) registerKeyedBackground() func() {
	r, ok := b.mgr.(keyedBackgroundRegistrar)
	if !ok {
		return func() {}
	}
	return r.RegisterKeyedBackgroundHandler(b.agentID, b)
}

// parseSlackSessionKey reverses slackSessionKey for this bot's agent.
func (b *Bot) parseSlackSessionKey(agentID, sessionKey string) (channel, threadTS string, ok bool) {
	if agentID != b.agentID {
		return "", "", false
	}
	suffix, ok := strings.CutPrefix(sessionKey, b.agentID+":slack:")
	if !ok {
		return "", "", false
	}
	channel, threadTS, ok = strings.Cut(suffix, ":")
	if !ok || channel == "" || threadTS == "" {
		return "", "", false
	}
	return channel, threadTS, true
}

// HandleKeyedBackgroundTurn posts an unsolicited background turn into its
// Slack thread. It opens a synthetic admission on the same thread FIFO as
// user turns (precedent: resumeGoal) so posts never interleave, registers an
// active turn so !stop works, and reuses the regular delivery pipeline.
func (b *Bot) HandleKeyedBackgroundTurn(agentID, sessionKey string, events <-chan agent.ChatEvent, cancel func()) {
	defer func() {
		for range events {
		}
	}()
	channel, threadTS, ok := b.parseSlackSessionKey(agentID, sessionKey)
	if !ok || b.ctx.Err() != nil {
		if cancel != nil {
			cancel()
		}
		return
	}
	reservation := b.reserveThread(channel, threadTS)
	turnCtx, turnCancel := context.WithCancel(b.ctx)
	active := b.registerActiveTurn(channel, threadTS, func() {
		if cancel != nil {
			cancel()
		}
		turnCancel()
	})
	defer func() {
		b.finishActiveTurn(channel, threadTS, active)
		turnCancel()
		b.finishStopTransaction(channel, threadTS, active)
		b.releaseThreadReservation(channel, threadTS, reservation)
	}()
	reservation.Wait()
	if turnCtx.Err() != nil {
		if cancel != nil {
			cancel()
		}
		return
	}
	b.deliverAgentTurn(turnCtx, slackTurnDelivery{
		channel:    channel,
		threadTS:   threadTS,
		sessionKey: sessionKey,
		turnCtx:    turnCtx,
		turnCancel: turnCancel,
		active:     active,
		userTurn:   false,
	}, events)
}

// KeyedBackgroundTasksAbandoned posts a best-effort notice when a lingering
// thread session was closed while background tasks were still running.
func (b *Bot) KeyedBackgroundTasksAbandoned(agentID, sessionKey string, pending int, reason string) {
	channel, threadTS, ok := b.parseSlackSessionKey(agentID, sessionKey)
	if !ok || pending <= 0 {
		return
	}
	msg := fmt.Sprintf("_バックグラウンド処理 %d件 が完了前に終了しました", pending)
	if reason != "" {
		msg += "（" + reason + "）"
	}
	msg += "。必要なら改めて依頼してください_"
	// Queue behind any in-flight reply on the thread so the notice never
	// interleaves with streamed or chunked output.
	reservation := b.reserveThread(channel, threadTS)
	defer b.releaseThreadReservation(channel, threadTS, reservation)
	reservation.Wait()
	ctx, c := context.WithTimeout(context.Background(), chunkPostTimeoutBase)
	defer c()
	b.postMessage(ctx, channel, threadTS, msg)
}
