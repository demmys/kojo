package agent

import (
	"context"
	"fmt"
	"strings"
)

// WebUI thread rooms as a keyed background surface: a thread turn runs on a
// keyed lingering claude session (SessionKey "groupdm:<roomID>"), so a
// run_in_background task started by it survives the turn. Its completion turn
// arrives here and is posted into the same room exactly like a normal thread
// reply (consumeThreadTurn), serialized through the room's thread FIFO.

// threadBackgroundPendingNote is appended to a thread reply whose session
// keeps lingering for still-running background tasks.
func threadBackgroundPendingNote(n int) string {
	return fmt.Sprintf("_バックグラウンド処理 %d件 実行中。完了したらこのスレッドで続きを投稿します_", n)
}

// threadBackgroundAbandonedNote is the notice posted when a lingering thread
// session ended with background tasks still pending.
func threadBackgroundAbandonedNote(pending int, reason string) string {
	if reason == KeyedStopRequestedReason {
		return fmt.Sprintf("_バックグラウンド処理 %d件 を停止しました（エージェントの依頼）_", pending)
	}
	msg := fmt.Sprintf("_バックグラウンド処理 %d件 が完了前に終了しました", pending)
	if reason != "" {
		msg += "（" + reason + "）"
	}
	return msg + "。必要なら改めて依頼してください_"
}

// liveAgentThread reports whether groupID is a live thread room of agentID.
func (m *GroupDMManager) liveAgentThread(groupID, agentID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, err := m.liveGroupLocked(groupID)
	return err == nil && isThreadRoom(g) && len(g.Members) == 1 && g.Members[0].AgentID == agentID
}

// HandleKeyedBackgroundTurn implements KeyedBackgroundHandler for WebUI thread
// rooms: it waits for the room's thread FIFO, exposes the turn as the room's
// live/stoppable turn, and posts the result daemon-authored.
func (m *GroupDMManager) HandleKeyedBackgroundTurn(agentID, sessionKey string, events <-chan ChatEvent, cancel func()) {
	defer func() {
		for range events {
		}
	}()
	abort := func() {
		if cancel != nil {
			cancel()
		}
	}
	groupID, ok := strings.CutPrefix(sessionKey, webUIThreadKeyPrefix)
	if !ok || groupID == "" || !m.liveAgentThread(groupID, agentID) {
		abort()
		return
	}
	reservation := m.threadTurns.Reserve(groupID)
	defer reservation.Release()
	reservation.Wait()
	if !m.liveAgentThread(groupID, agentID) {
		abort()
		return
	}

	ctx, ctxCancel := context.WithCancel(context.Background())
	defer ctxCancel()
	m.threadCancelMu.Lock()
	m.threadCancels[groupID] = func() {
		ctxCancel()
		abort()
	}
	delete(m.threadStopped, groupID)
	m.threadCancelMu.Unlock()
	defer func() {
		m.threadCancelMu.Lock()
		delete(m.threadCancels, groupID)
		delete(m.threadStopped, groupID)
		m.threadCancelMu.Unlock()
	}()

	var agentModel, agentEffort string
	if a, ok := m.threadAgentInfo(agentID); ok {
		agentModel = a.Model
		agentEffort = a.Effort
	}
	m.startThreadLive(groupID, agentModel, agentEffort)
	defer m.endThreadLive(groupID)

	// A lingering keyed session only exists on the node running the agent, so
	// files the background turn stages are captured locally.
	replyMessageID := generateGroupMessageID()
	attachmentStageDir := threadAttachmentStageDir(agentID, groupID)
	attachmentWatcher := m.agentMgr.watchAndStreamAttachmentsFromDir(ctx, agentID, replyMessageID, attachmentStageDir)
	m.consumeThreadTurn(ctx, events, threadTurnOutput{
		agentID:            agentID,
		groupID:            groupID,
		payload:            "[background task notification]",
		agentModel:         agentModel,
		agentEffort:        agentEffort,
		replyMessageID:     replyMessageID,
		attachmentStageDir: attachmentStageDir,
		attachmentWatcher:  attachmentWatcher,
	})
}

// KeyedBackgroundTasksAbandoned implements KeyedBackgroundHandler: it posts a
// notice into the thread (behind any in-flight reply) when the lingering
// session ended before its background tasks completed.
func (m *GroupDMManager) KeyedBackgroundTasksAbandoned(agentID, sessionKey string, pending int, reason string) {
	groupID, ok := strings.CutPrefix(sessionKey, webUIThreadKeyPrefix)
	if !ok || groupID == "" || pending <= 0 || !m.liveAgentThread(groupID, agentID) {
		return
	}
	reservation := m.threadTurns.Reserve(groupID)
	defer reservation.Release()
	reservation.Wait()
	if _, err := m.postThreadReply(groupID, agentID, threadBackgroundAbandonedNote(pending, reason), "", "", nil, "", nil, false); err != nil {
		m.logger.Warn("failed to post thread background abandoned notice", "group", groupID, "agent", agentID, "err", err)
	}
}
