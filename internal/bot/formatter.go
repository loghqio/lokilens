package bot

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/slack-go/slack"
)

// mdHeadingPattern matches Markdown headings (## Heading) at the start of a line.
// Slack renders these as literal "## text" — we convert to bold instead.
var mdHeadingPattern = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)

// mdLinkPattern matches Markdown links [text](url) so we can convert to Slack format <url|text>.
// Only matches http/https URLs to avoid false positives on log content like [error] or [REDACTED].
var mdLinkPattern = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^\)]+)\)`)

// Slack's section block text limit is 3000 characters.
const maxSectionTextLength = 2900

// maxFallbackLength caps the fallback text used in Slack notifications and push
// alerts. Without this, a 5,000-char agent response becomes an unreadable wall
// of text on the engineer's phone at 3am.
const maxFallbackLength = 150

// runeAlignedCutoff backs up from a byte position to the nearest valid UTF-8
// rune boundary. Prevents slicing multibyte characters (¥, €, emoji) in half,
// which would produce garbled text in push notifications at 3am.
func runeAlignedCutoff(text string, pos int) int {
	if pos >= len(text) {
		return len(text)
	}
	for pos > 0 && !utf8.RuneStart(text[pos]) {
		pos--
	}
	return pos
}

// TruncateFallback shortens text for use as Slack's notification/fallback text.
// Cuts at the last space before the limit and appends "…" to avoid mid-word breaks.
func TruncateFallback(text string) string {
	// Always cut at first newline — push notifications should be single-line
	if nl := strings.IndexByte(text, '\n'); nl >= 0 {
		text = text[:nl]
		if len(text) <= maxFallbackLength {
			// Don't add "…" if the text already ends with an ellipsis — avoids
			// "247 errors found...…" on push notifications at 3am.
			if strings.HasSuffix(text, "…") || strings.HasSuffix(text, "...") {
				return text
			}
			// Don't add "…" if the line ends with sentence-ending punctuation —
			// "No errors found.…" looks unprofessional on a 3am push notification.
			// The "…" is meant to signal truncation, but a complete sentence
			// already reads naturally on its own.
			if endsWithSentencePunctuation(text) {
				return text
			}
			return text + "…"
		}
	}
	if len(text) <= maxFallbackLength {
		return text
	}
	cutoff := strings.LastIndex(text[:maxFallbackLength], " ")
	if cutoff < maxFallbackLength/2 {
		cutoff = runeAlignedCutoff(text, maxFallbackLength)
	}
	return text[:cutoff] + "…"
}

// sanitizeForSlack converts standard Markdown artifacts that Gemini may emit
// into Slack mrkdwn equivalents. Without this, **bold**, ## headings, and
// [links](url) render as literal characters in Slack.
func sanitizeForSlack(text string) string {
	// Convert **bold** → *bold* (Slack uses single asterisks)
	// Must replace before any other processing to avoid double-matching.
	text = strings.ReplaceAll(text, "****", "") // degenerate empty bold
	for strings.Contains(text, "**") {
		start := strings.Index(text, "**")
		end := strings.Index(text[start+2:], "**")
		if end == -1 {
			break // unmatched — leave as-is
		}
		end += start + 2
		inner := text[start+2 : end]
		text = text[:start] + "*" + inner + "*" + text[end+2:]
	}

	// Convert ## Heading → *Heading* on its own line
	text = mdHeadingPattern.ReplaceAllString(text, "*$1*")

	// Convert [text](url) → <url|text> (Slack link format)
	// Only matches http/https URLs to avoid false positives on [REDACTED] or [error].
	text = mdLinkPattern.ReplaceAllString(text, "<$2|$1>")

	return text
}

// FormatResponse converts agent response text into Slack Block Kit blocks.
// durationMS is optional (pass 0 to omit timing from the footer).
func FormatResponse(text, userID string, durationMS ...int64) []slack.Block {
	var blocks []slack.Block

	// Guard against empty or whitespace-only text — Slack rejects empty section blocks
	if strings.TrimSpace(text) == "" {
		text = "_No response generated._"
	}

	// Sanitize Markdown artifacts that the LLM may emit despite instruction
	text = sanitizeForSlack(text)

	// Split long responses into multiple section blocks
	for len(text) > 0 {
		chunk := text
		if len(chunk) > maxSectionTextLength {
			// Cut at last newline before the limit to avoid mid-word/mid-line breaks
			cutoff := strings.LastIndex(chunk[:maxSectionTextLength], "\n")
			if cutoff < maxSectionTextLength/2 {
				// No good newline — try last space to avoid breaking emoji/formatting tokens
				cutoff = strings.LastIndex(chunk[:maxSectionTextLength], " ")
			}
			if cutoff < maxSectionTextLength/2 {
				cutoff = runeAlignedCutoff(chunk, maxSectionTextLength)
			}
			chunk = text[:cutoff]
			text = text[cutoff:]

			// If we split inside a code block, close it in this chunk and
			// reopen it in the next. Without this, an unclosed ``` causes
			// all remaining text to render as code — unreadable at 3am.
			if strings.Count(chunk, "```")%2 != 0 {
				chunk += "\n```"
				text = "```\n" + text
			}
		} else {
			text = ""
		}

		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, chunk, false, false),
			nil, nil,
		))
	}

	// Slack limits blocks to 50 per message; reserve 3 for truncation notice + divider + context
	if len(blocks) > 47 {
		blocks = blocks[:47]
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, "_...response truncated_", false, false),
			nil, nil,
		))
	}

	// Divider
	blocks = append(blocks, slack.NewDividerBlock())

	// Context footer with optional timing
	footer := fmt.Sprintf("Asked by <@%s>", userID)
	if len(durationMS) > 0 && durationMS[0] > 0 {
		dur := durationMS[0]
		if dur >= 1000 {
			footer += fmt.Sprintf(" | %.1fs", float64(dur)/1000.0)
		} else {
			footer += fmt.Sprintf(" | %dms", dur)
		}
	}
	footer += " | LokiLens"
	blocks = append(blocks, slack.NewContextBlock("",
		slack.NewTextBlockObject(slack.MarkdownType, footer, false, false),
	))

	return blocks
}

// FormatQuickReply creates a lightweight block for quick replies (greetings,
// thanks, dismissals). Unlike FormatResponse, this omits the divider and footer
// — a "You're welcome!" doesn't need "Asked by <@user> | LokiLens" cluttering
// the thread at 3am.
func FormatQuickReply(text string) []slack.Block {
	return []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, text, false, false),
			nil, nil,
		),
	}
}

// FormatError creates error message blocks.
func FormatError(errMsg string) []slack.Block {
	return []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType,
				fmt.Sprintf(":warning: %s", errMsg),
				false, false),
			nil, nil,
		),
	}
}

// endsWithSentencePunctuation returns true if text ends with punctuation that
// signals a complete thought (period, exclamation, question mark). Used by
// TruncateFallback to avoid appending "…" to sentences that already read
// naturally — "No errors found.…" looks broken on a push notification.
func endsWithSentencePunctuation(text string) bool {
	if text == "" {
		return false
	}
	last := text[len(text)-1]
	if last != '.' && last != '!' && last != '?' {
		return false
	}
	// Don't match trailing ellipsis ("...") — that's handled separately
	if last == '.' && len(text) >= 3 && text[len(text)-3:] == "..." {
		return false
	}
	return true
}

// StripMention removes the <@BOTID> mention prefix from message text.
func StripMention(text string) string {
	text = strings.TrimSpace(text)
	// Only strip if the text starts with a Slack mention like <@U12345678>
	if !strings.HasPrefix(text, "<@") {
		return text
	}
	if idx := strings.Index(text, "> "); idx != -1 {
		return strings.TrimSpace(text[idx+2:])
	}
	if idx := strings.Index(text, ">"); idx != -1 {
		return strings.TrimSpace(text[idx+1:])
	}
	return text
}
