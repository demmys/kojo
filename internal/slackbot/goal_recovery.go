package slackbot

import "strings"

func (b *Bot) resumeGoal(key string) {
	// The suffix after newline is an internally supplied guarded resume
	// command. The stable session key itself can never contain a newline.
	sessionKey, command, ok := strings.Cut(key, "\n")
	if !ok {
		return
	}
	suffix, ok := strings.CutPrefix(sessionKey, b.agentID+":slack:")
	if !ok {
		return
	}
	channel, thread, ok := strings.Cut(suffix, ":")
	if !ok || channel == "" {
		return
	}
	b.processIncomingWithAttachmentsMode(b.ctx, channel, thread, "", command, "", nil, false)
}
