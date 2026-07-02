package slackbot

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Slack mrkdwn uses different formatting from standard Markdown.
// These helpers convert between the two.

var (
	// Slack link format: <URL|text> or <URL>
	reLinkWithText = regexp.MustCompile(`<(https?://[^|>]+)\|([^>]+)>`)
	reLinkBare     = regexp.MustCompile(`<(https?://[^>]+)>`)

	// Slack user/channel mentions: <@U12345> or <#C12345|channel-name>
	reUserMention    = regexp.MustCompile(`<@([A-Z0-9]+)>`)
	reChannelMention = regexp.MustCompile(`<#[A-Z0-9]+\|([^>]+)>`)
)

// UserResolver resolves a Slack user ID to a display name.
// Return the original ID if resolution fails.
type UserResolver func(userID string) string

// SlackToPlain converts Slack mrkdwn to plain text suitable for the agent.
// It resolves links and strips mention formatting.
// If resolve is non-nil, user mentions <@U12345> are resolved to display names.
func SlackToPlain(text string, resolve UserResolver) string {
	// Replace links: <url|text> → text (url)
	text = reLinkWithText.ReplaceAllString(text, "$2 ($1)")
	// Replace bare links: <url> → url
	text = reLinkBare.ReplaceAllString(text, "$1")
	// Replace channel mentions: <#C123|general> → #general
	text = reChannelMention.ReplaceAllString(text, "#$1")
	// Resolve user mentions: <@U12345> → @DisplayName
	text = reUserMention.ReplaceAllStringFunc(text, func(match string) string {
		id := reUserMention.FindStringSubmatch(match)[1]
		if resolve != nil {
			return "@" + resolve(id)
		}
		return "@" + id
	})
	return text
}

// StripBotMention removes all @bot mentions from the message text.
func StripBotMention(text, botUserID string) string {
	mention := "<@" + botUserID + ">"
	text = strings.ReplaceAll(text, mention, "")
	text = strings.TrimSpace(text)
	return text
}

// SplitMessage splits a long message into chunks that fit within Slack's
// message length limit (approximately 3000 chars per chunk for safety).
func SplitMessage(text string, maxLen int) []string {
	if maxLen <= 0 {
		maxLen = 3000
	}
	if len(text) <= maxLen {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}
		// Try to split at a paragraph boundary
		cut := maxLen
		if idx := strings.LastIndex(text[:maxLen], "\n\n"); idx > maxLen/2 {
			cut = idx + 2
		} else if idx := strings.LastIndex(text[:maxLen], "\n"); idx > maxLen/2 {
			cut = idx + 1
		}
		// Ensure cut falls on a UTF-8 rune boundary to avoid splitting
		// multi-byte characters (e.g. Japanese text at 3 bytes per char).
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	return chunks
}
