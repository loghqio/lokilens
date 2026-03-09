package bot

import (
	"fmt"
	"strings"

	"github.com/slack-go/slack"
)

// Slack's section block text limit is 3000 characters.
const maxSectionTextLength = 2900

// FormatResponse converts agent response text into Slack Block Kit blocks.
func FormatResponse(text, userID string) []slack.Block {
	var blocks []slack.Block

	// Split long responses into multiple section blocks
	for len(text) > 0 {
		chunk := text
		if len(chunk) > maxSectionTextLength {
			// Cut at last newline before the limit to avoid mid-word/mid-line breaks
			cutoff := strings.LastIndex(chunk[:maxSectionTextLength], "\n")
			if cutoff < maxSectionTextLength/2 {
				cutoff = maxSectionTextLength // no good newline, hard cut
			}
			chunk = text[:cutoff]
			text = text[cutoff:]
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

	// Context footer
	blocks = append(blocks, slack.NewContextBlock("",
		slack.NewTextBlockObject(slack.MarkdownType,
			fmt.Sprintf("Asked by <@%s> | LokiLens", userID),
			false, false),
	))

	return blocks
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
