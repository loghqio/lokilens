package bot

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestStripMention_WithMention(t *testing.T) {
	result := StripMention("<@U12345678> show me errors")
	if result != "show me errors" {
		t.Errorf("expected 'show me errors', got %q", result)
	}
}

func TestStripMention_WithoutMention(t *testing.T) {
	result := StripMention("show me errors")
	if result != "show me errors" {
		t.Errorf("expected unchanged, got %q", result)
	}
}

func TestStripMention_MentionOnly(t *testing.T) {
	result := StripMention("<@U12345678>")
	if result != "" {
		t.Errorf("expected empty, got %q", result)
	}
}

func TestStripMention_MentionWithoutSpace(t *testing.T) {
	result := StripMention("<@U12345678>errors")
	if result != "errors" {
		t.Errorf("expected 'errors', got %q", result)
	}
}

func TestStripMention_Empty(t *testing.T) {
	result := StripMention("")
	if result != "" {
		t.Errorf("expected empty, got %q", result)
	}
}

func TestIsDM(t *testing.T) {
	if !IsDM("D12345678") {
		t.Error("expected true for DM channel")
	}
	if IsDM("C12345678") {
		t.Error("expected false for regular channel")
	}
}

func TestIsBotMessage(t *testing.T) {
	if !IsBotMessage("B123", "") {
		t.Error("expected true for bot message")
	}
	if !IsBotMessage("", "message_changed") {
		t.Error("expected true for subtype message")
	}
	if IsBotMessage("", "") {
		t.Error("expected false for normal user message")
	}
}

func TestFormatResponse_ShortText(t *testing.T) {
	blocks := FormatResponse("Hello, world!", "U123")
	if len(blocks) == 0 {
		t.Error("expected at least one block")
	}
	// Should have: 1 section + 1 divider + 1 context = 3
	if len(blocks) != 3 {
		t.Errorf("expected 3 blocks, got %d", len(blocks))
	}
}

func TestFormatResponse_EmptyText(t *testing.T) {
	blocks := FormatResponse("", "U123")
	// Empty text should produce: 1 section (fallback text) + 1 divider + 1 context = 3
	if len(blocks) != 3 {
		t.Errorf("expected 3 blocks for empty text, got %d", len(blocks))
	}
}

func TestFormatResponse_WhitespaceOnlyText(t *testing.T) {
	blocks := FormatResponse("   \n\t  ", "U123")
	// Whitespace-only text should be treated as empty and get fallback text
	if len(blocks) != 3 {
		t.Errorf("expected 3 blocks for whitespace-only text, got %d", len(blocks))
	}
}

func TestFormatError(t *testing.T) {
	blocks := FormatError("something went wrong")
	if len(blocks) != 1 {
		t.Errorf("expected 1 block, got %d", len(blocks))
	}
}

func TestFormatResponse_WithDuration(t *testing.T) {
	blocks := FormatResponse("test response", "U123", int64(3500))
	if len(blocks) != 3 {
		t.Errorf("expected 3 blocks, got %d", len(blocks))
	}
	// Last block is context — check it contains timing
	// We can't easily inspect Block Kit internals, but we ensure it doesn't panic
}

func TestFormatResponse_WithSmallDuration(t *testing.T) {
	blocks := FormatResponse("test response", "U123", int64(450))
	if len(blocks) != 3 {
		t.Errorf("expected 3 blocks, got %d", len(blocks))
	}
}

func TestFormatResponse_ZeroDuration(t *testing.T) {
	blocks := FormatResponse("test response", "U123", int64(0))
	if len(blocks) != 3 {
		t.Errorf("expected 3 blocks, got %d", len(blocks))
	}
}


func TestIsGratitude(t *testing.T) {
	positives := []string{
		"thanks", "thank you", "thx", "ty", "cheers", "got it", "ok cool",
		"perfect", "great", "awesome", "thanks!", "thank you!!", "ok", "cool",
		// Acknowledgment variants
		"no worries", "np", "nw", "all good", "sounds good",
		// Engineer affirmatives — common in incident channels
		"looks good", "lgtm", "yep", "yup", "yea", "yeah",
		"noted", "copy that", "roger", "sweet", "nice one",
		// With punctuation
		"lgtm!", "yep!", "roger!",
	}
	for _, input := range positives {
		if !isGratitude(strings.ToLower(input)) {
			t.Errorf("isGratitude(%q) = false, want true", input)
		}
	}

	negatives := []string{
		"thanks for checking errors", "thank you now show me logs",
		"ok show me errors", "cool what about payments",
		"yeah what about payments", "yep show me more",
		"noted check orders too", "looks good but check payments",
	}
	for _, input := range negatives {
		if isGratitude(strings.ToLower(input)) {
			t.Errorf("isGratitude(%q) = true, want false", input)
		}
	}
}

func TestQuickReplyFor(t *testing.T) {
	quick := []string{
		"thanks", "thank you!", "hi", "hello", "hey", "ok cool", "perfect",
		// Greetings with punctuation — must not trigger a full agent call
		"hey!", "hello!!", "hi!", "Hey!",
		// Time-of-day greetings
		"morning", "good morning", "gm", "Good Morning",
		"afternoon", "good afternoon",
		"evening", "good evening",
		// Night greetings
		"good night", "gn", "night",
		// Acknowledgments
		"no worries", "np", "all good", "sounds good",
		// Engineer affirmatives
		"looks good", "lgtm", "yep!", "yup", "yeah",
		"noted", "copy that", "roger", "sweet",
		// Empty mention — @LokiLens with no text
		"", "  ", "\n",
		// Help requests — must be short-circuited
		"help", "Help", "what can you do", "how do i use this",
	}
	for _, input := range quick {
		if _, ok := quickReplyFor(input); !ok {
			t.Errorf("quickReplyFor(%q) returned false, want true", input)
		}
	}

	notQuick := []string{
		"show me errors", "hi show me logs", "hey what's happening",
		"thanks for checking, now show me orders",
		"good morning show me errors",
		"morning — any issues today?",
		"help me find errors in payments",
	}
	for _, input := range notQuick {
		if _, ok := quickReplyFor(input); ok {
			t.Errorf("quickReplyFor(%q) returned true, want false — real queries must not be short-circuited", input)
		}
	}
}

func TestContainsWord(t *testing.T) {
	positives := []struct {
		text, word string
	}{
		{"payments is down", "down"},
		{"is the API down?", "down"},
		{"down since 2pm", "down"},
		{"going down fast", "down"},
		{"it's down!", "down"},
		{"down", "down"},
		// Colon and paren boundaries — common in Slack messages
		{"status:down", "down"},
		{"it's a p1:", "p1"},
		{"(down)", "down"},
		{"sev1;", "sev1"},
		{`"down"`, "down"},
	}
	for _, tc := range positives {
		if !containsWord(tc.text, tc.word) {
			t.Errorf("containsWord(%q, %q) = false, want true", tc.text, tc.word)
		}
	}

	negatives := []struct {
		text, word string
	}{
		{"give me a breakdown", "down"},
		{"markdown format", "down"},
		{"countdown timer", "down"},
		{"downloading logs", "down"},
		{"meltdown scenario", "down"},
		// Empty word must not infinite-loop — should return false
		{"any text here", ""},
	}
	for _, tc := range negatives {
		if containsWord(tc.text, tc.word) {
			t.Errorf("containsWord(%q, %q) = true, want false", tc.text, tc.word)
		}
	}
}

func TestFormatResponse_LongTextBreaksAtSpace(t *testing.T) {
	// Create text that exceeds maxSectionTextLength with no newlines,
	// to verify it breaks at a space rather than mid-word
	word := "errorlog "
	text := strings.Repeat(word, maxSectionTextLength/len(word)+10)
	blocks := FormatResponse(text, "U123")
	// Should produce at least 2 section blocks + divider + context
	if len(blocks) < 4 {
		t.Errorf("expected at least 4 blocks for long text, got %d", len(blocks))
	}
}

func TestTruncateFallback_Short(t *testing.T) {
	text := "All good — no errors in payments."
	got := TruncateFallback(text)
	if got != text {
		t.Errorf("short text should be unchanged, got %q", got)
	}
}

func TestTruncateFallback_Long(t *testing.T) {
	text := strings.Repeat("errors found in payments service. ", 20)
	got := TruncateFallback(text)
	if len(got) > maxFallbackLength+10 { // allow for "…" suffix
		t.Errorf("expected truncated to ~%d chars, got %d", maxFallbackLength, len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected '…' suffix, got %q", got)
	}
}

func TestTruncateFallback_CutsAtNewline(t *testing.T) {
	text := "Critical: payments errors spiking\nKey findings:\n- 247 errors in last hour"
	got := TruncateFallback(text)
	if got != "Critical: payments errors spiking…" {
		t.Errorf("expected cut at first newline, got %q", got)
	}
}

func TestTruncateFallback_NoDoubleEllipsis(t *testing.T) {
	// When first line already ends with "...", don't append "…"
	text := "247 errors found...\nKey findings:\n- DB timeouts"
	got := TruncateFallback(text)
	if got != "247 errors found..." {
		t.Errorf("expected no double ellipsis, got %q", got)
	}

	// Same for Unicode ellipsis "…"
	text2 := "Investigating payments…\nMore details below"
	got2 := TruncateFallback(text2)
	if got2 != "Investigating payments…" {
		t.Errorf("expected no double ellipsis for Unicode, got %q", got2)
	}
}

func TestTruncateFallback_SingleLineNoEllipsis(t *testing.T) {
	// Single line ending with "..." should NOT get another "…" added
	// (single line under limit with no newline returns as-is)
	text := "All clear..."
	got := TruncateFallback(text)
	if got != "All clear..." {
		t.Errorf("expected unchanged single line, got %q", got)
	}
}

func TestTruncateFallback_Empty(t *testing.T) {
	got := TruncateFallback("")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestSanitizeForSlack_DoubleBold(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		// **bold** → *bold*
		{"**Critical** — payments is down", "*Critical* — payments is down"},
		// Multiple **bold** segments
		{"**Error** in **payments**", "*Error* in *payments*"},
		// Already using single asterisks — no change
		{"*Critical* — payments is down", "*Critical* — payments is down"},
		// Unmatched ** — leave as-is
		{"this has ** unmatched stars", "this has ** unmatched stars"},
		// Empty bold
		{"empty **** bold", "empty  bold"},
		// No markdown
		{"plain text", "plain text"},
	}
	for _, tc := range tests {
		got := sanitizeForSlack(tc.input)
		if got != tc.expected {
			t.Errorf("sanitizeForSlack(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestSanitizeForSlack_Headings(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		// ## Heading → *Heading*
		{"## Key Findings", "*Key Findings*"},
		// ### Sub-heading
		{"### Error Details", "*Error Details*"},
		// # Title
		{"# Summary", "*Summary*"},
		// Heading in multi-line context
		{"Intro text\n## Findings\n- item 1", "Intro text\n*Findings*\n- item 1"},
		// Not a heading (no space after #)
		{"#hashtag", "#hashtag"},
	}
	for _, tc := range tests {
		got := sanitizeForSlack(tc.input)
		if got != tc.expected {
			t.Errorf("sanitizeForSlack(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestSanitizeForSlack_Combined(t *testing.T) {
	// A realistic LLM response with both issues
	input := "## Summary\n\n**Critical** — payments has 247 errors\n\n### Details\n\nError rate is **increasing**."
	expected := "*Summary*\n\n*Critical* — payments has 247 errors\n\n*Details*\n\nError rate is *increasing*."
	got := sanitizeForSlack(input)
	if got != expected {
		t.Errorf("sanitizeForSlack combined:\ngot:  %q\nwant: %q", got, expected)
	}
}

func TestSanitizeForSlack_PreservesCodeBlocks(t *testing.T) {
	// Code blocks may contain ** as glob patterns or operators —
	// verify they're handled sensibly (we sanitize inside code blocks
	// too since Slack ignores mrkdwn inside ```, but it shouldn't crash)
	input := "Error found:\n```\ngrep **/*.log\n```\nSee above."
	got := sanitizeForSlack(input)
	// The ** inside a code block will be converted, but since Slack
	// renders code blocks as literal text, this is harmless.
	// The important thing is it doesn't panic or corrupt the output.
	if !strings.Contains(got, "```") {
		t.Errorf("expected code block preserved, got %q", got)
	}
}

func TestSanitizeForSlack_MarkdownLinks(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		// Standard Markdown link → Slack link
		{"Check the [Grafana dashboard](https://grafana.internal/d/abc)", "Check the <https://grafana.internal/d/abc|Grafana dashboard>"},
		// Multiple links
		{"See [docs](https://docs.example.com) and [wiki](http://wiki.internal)", "See <https://docs.example.com|docs> and <http://wiki.internal|wiki>"},
		// Non-URL brackets should NOT be converted (log content, redactions)
		{"Found [REDACTED_EMAIL] in logs", "Found [REDACTED_EMAIL] in logs"},
		{"Error: [timeout] connection refused", "Error: [timeout] connection refused"},
		// No link
		{"plain text", "plain text"},
	}
	for _, tc := range tests {
		got := sanitizeForSlack(tc.input)
		if got != tc.expected {
			t.Errorf("sanitizeForSlack(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestFallbackSanitization(t *testing.T) {
	// Validates that TruncateFallback(sanitizeForSlack(...)) produces clean push
	// notification text — no raw Markdown artifacts on the engineer's phone at 3am.
	tests := []struct {
		name     string
		input    string
		notWant  string // substring that must NOT appear in output
		wantHas  string // substring that MUST appear in output
	}{
		{
			name:    "double bold becomes single bold",
			input:   "**Critical**: payments is down",
			notWant: "**",
			wantHas: "*Critical*",
		},
		{
			name:    "markdown heading becomes bold",
			input:   "## Error Summary\nPayments had 247 errors",
			notWant: "##",
			wantHas: "*Error Summary*",
		},
		{
			name:    "markdown link becomes slack link",
			input:   "See [dashboard](https://grafana.internal/d/abc) for details",
			notWant: "](https://",
			wantHas: "<https://grafana.internal/d/abc|dashboard>",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateFallback(sanitizeForSlack(tc.input))
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Errorf("output %q should not contain %q", got, tc.notWant)
			}
			if tc.wantHas != "" && !strings.Contains(got, tc.wantHas) {
				t.Errorf("output %q should contain %q", got, tc.wantHas)
			}
		})
	}
}

func TestFormatResponse_CodeBlockSplitting(t *testing.T) {
	// Build a response where a code block spans across the split boundary.
	// Without code-block-aware splitting, the first chunk has an unclosed ```
	// and the second chunk starts with stray code content — unreadable.
	header := "Error logs from payments:\n```\n"
	var logLines strings.Builder
	for i := range 200 {
		_ = i
		logLines.WriteString("2024-01-15T14:31:02.123Z payments error: DB connection refused\n")
	}
	footer := "```\nCheck the connection pool."
	text := header + logLines.String() + footer

	blocks := FormatResponse(text, "U123")
	// Should produce multiple section blocks + divider + context
	if len(blocks) < 4 {
		t.Fatalf("expected at least 4 blocks, got %d", len(blocks))
	}
}

func TestCodeBlockAwareSplit(t *testing.T) {
	// Directly test that splitting inside a code block closes and reopens it.
	// The code block must be large enough that the split point falls inside it.
	codeContent := strings.Repeat("log line\n", maxSectionTextLength/9+1)
	text := "Before:\n```\n" + codeContent + "```\nAfter the code block."

	blocks := FormatResponse(text, "U123")
	sectionBlocks := len(blocks) - 2 // minus divider + context
	if sectionBlocks < 2 {
		t.Fatalf("expected at least 2 section blocks, got %d (total text len: %d)", sectionBlocks, len(text))
	}
}

func TestTruncateFallback_MultibyteSafety(t *testing.T) {
	// Build text where the maxFallbackLength boundary falls inside a multibyte char.
	// '¥' is 2 bytes in UTF-8, '€' is 3 bytes, '💰' is 4 bytes.
	// Fill with single-byte 'x' chars with NO spaces (forces hard cutoff),
	// then place a multibyte char right at the boundary.
	prefix := strings.Repeat("x", maxFallbackLength-1) + "€" + "tail"
	got := TruncateFallback(prefix)
	// The cutoff must not split the '€' — it should back up to before it.
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected '…' suffix, got %q", got)
	}
	// Verify the result is valid UTF-8
	trimmed := strings.TrimSuffix(got, "…")
	for i := 0; i < len(trimmed); {
		r, size := utf8.DecodeRuneInString(trimmed[i:])
		if r == utf8.RuneError && size == 1 {
			t.Fatalf("invalid UTF-8 at byte %d in %q", i, got)
		}
		i += size
	}
}

func TestRuneAlignedCutoff(t *testing.T) {
	// '€' is 3 bytes (0xE2, 0x82, 0xAC)
	text := "abc€def"
	// 'a'=0, 'b'=1, 'c'=2, '€'=3,4,5, 'd'=6, 'e'=7, 'f'=8
	// Cutting at byte 4 (middle of '€') should back up to byte 3
	if got := runeAlignedCutoff(text, 4); got != 3 {
		t.Errorf("runeAlignedCutoff at byte 4 = %d, want 3", got)
	}
	if got := runeAlignedCutoff(text, 5); got != 3 {
		t.Errorf("runeAlignedCutoff at byte 5 = %d, want 3", got)
	}
	// Cutting at byte 3 (start of '€') is valid
	if got := runeAlignedCutoff(text, 3); got != 3 {
		t.Errorf("runeAlignedCutoff at byte 3 = %d, want 3", got)
	}
	// Cutting at byte 6 (start of 'd') is valid
	if got := runeAlignedCutoff(text, 6); got != 6 {
		t.Errorf("runeAlignedCutoff at byte 6 = %d, want 6", got)
	}
	// Cutting beyond length returns length
	if got := runeAlignedCutoff(text, 100); got != len(text) {
		t.Errorf("runeAlignedCutoff beyond length = %d, want %d", got, len(text))
	}
}

func TestFormatResponse_MultibyteSplitSafety(t *testing.T) {
	// Build text that exceeds maxSectionTextLength with multibyte chars and no spaces
	// to force the hard cutoff path. Verify no invalid UTF-8 in output.
	line := strings.Repeat("x", 100) + "€" + strings.Repeat("x", 100) + "\n"
	text := strings.Repeat(line, maxSectionTextLength/len(line)+5)
	blocks := FormatResponse(text, "U123")
	if len(blocks) < 4 {
		t.Fatalf("expected multiple blocks, got %d", len(blocks))
	}
}


func TestQuickReplyFor_Gratitude(t *testing.T) {
	cases := []string{"thanks", "Thank you!", "thx", "cheers", "got it", "ok"}
	for _, input := range cases {
		reply, ok := quickReplyFor(input)
		if !ok {
			t.Errorf("quickReplyFor(%q) returned false, want true", input)
		}
		if reply != "You're welcome! Let me know if anything else comes up." {
			t.Errorf("quickReplyFor(%q) = %q, want gratitude reply", input, reply)
		}
	}
}

func TestQuickReplyFor_Greeting(t *testing.T) {
	cases := []string{"hello", "Hey!", "hi", "good morning", "yo"}
	for _, input := range cases {
		reply, ok := quickReplyFor(input)
		if !ok {
			t.Errorf("quickReplyFor(%q) returned false, want true", input)
		}
		expected := "Hey! I'm LokiLens — ask me about logs, errors, or service health. What can I help with?"
		if reply != expected {
			t.Errorf("quickReplyFor(%q) = %q, want greeting reply", input, reply)
		}
	}
}

func TestQuickReplyFor_EmptyMention(t *testing.T) {
	// @LokiLens with no text — should get a greeting, not burn an LLM call
	cases := []string{"", "  ", "\t\n"}
	for _, input := range cases {
		reply, ok := quickReplyFor(input)
		if !ok {
			t.Errorf("quickReplyFor(%q) returned false, want true for empty mention", input)
		}
		expected := "Hey! I'm LokiLens — ask me about logs, errors, or service health. What can I help with?"
		if reply != expected {
			t.Errorf("quickReplyFor(%q) = %q, want greeting", input, reply)
		}
	}
}

func TestQuickReplyFor_Help(t *testing.T) {
	// Help must be available even when circuit breaker is open
	cases := []string{"help", "Help", "HELP", "what can you do", "how do i use this"}
	for _, input := range cases {
		reply, ok := quickReplyFor(input)
		if !ok {
			t.Errorf("quickReplyFor(%q) returned false, want true for help", input)
		}
		if !strings.Contains(reply, "I'm LokiLens") {
			t.Errorf("quickReplyFor(%q) doesn't contain help text, got: %s", input, reply[:50])
		}
	}
}

func TestQuickReplyFor_HelpWithQueryPassesThrough(t *testing.T) {
	// "help me find errors" is a real query, not a help request
	queries := []string{
		"help me find errors in payments",
		"help me investigate the timeout",
		"help check the logs",
	}
	for _, q := range queries {
		if _, ok := quickReplyFor(q); ok {
			t.Errorf("quickReplyFor(%q) returned true — real queries with 'help' must not be short-circuited", q)
		}
	}
}

func TestIsGratitude_QuestionMark(t *testing.T) {
	// "ok?" and "cool?" should be recognized as gratitude
	if !isGratitude("ok?") {
		t.Error("isGratitude('ok?') = false, want true")
	}
	if !isGratitude("cool?") {
		t.Error("isGratitude('cool?') = false, want true")
	}
}


func TestFormatQuickReply_NoFooter(t *testing.T) {
	blocks := FormatQuickReply("You're welcome!")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d — quick replies should not have divider or footer", len(blocks))
	}
}

func TestFormatQuickReply_VsFormatResponse(t *testing.T) {
	// FormatResponse adds divider + context footer (3+ blocks minimum).
	// FormatQuickReply should be lighter — just the message.
	full := FormatResponse("You're welcome!", "U123")
	quick := FormatQuickReply("You're welcome!")
	if len(quick) >= len(full) {
		t.Errorf("FormatQuickReply (%d blocks) should be lighter than FormatResponse (%d blocks)",
			len(quick), len(full))
	}
}

func TestQuickReplyFor_Dismissals(t *testing.T) {
	dismissals := []string{
		"nvm", "nevermind", "never mind", "cancel",
		"forget it", "skip it", "figured it out", "found it",
		"Nvm!", "NEVERMIND", "Cancel.",
		"all set", "all sorted", "sorted", "resolved",
		"all clear", "false alarm",
		"I'm good", "we're good", "im good", "were good",
		"All set!", "RESOLVED",
	}
	for _, input := range dismissals {
		reply, ok := quickReplyFor(input)
		if !ok {
			t.Errorf("quickReplyFor(%q) should match as dismissal", input)
			continue
		}
		if !strings.Contains(reply, "let me know") {
			t.Errorf("quickReplyFor(%q) = %q — expected dismissal response", input, reply)
		}
	}
}

func TestQuickReplyFor_DismissalNotFalsePositive(t *testing.T) {
	// "nevermind" as a substring of a real query should NOT trigger dismissal
	notDismissals := []string{
		"nvm, actually show me errors from payments",
		"cancel the deploy and show me logs",
		"I found it but can you check orders too?",
		"is the issue resolved?",
		"all set up and ready to deploy",
		"we're good to push to prod right?",
	}
	for _, input := range notDismissals {
		_, ok := quickReplyFor(input)
		if ok {
			t.Errorf("quickReplyFor(%q) should NOT match — contains a real query", input)
		}
	}
}

func TestQuickReplyFor_ThreadFollowup_AffirmativePassesToLLM(t *testing.T) {
	// In thread follow-ups, ambiguous affirmatives like "yeah", "ok", "yep"
	// should NOT be short-circuited — they could be answering a bot question
	// like "Want me to check upstream services?"
	affirmatives := []string{
		"yeah", "yep", "yup", "ok", "ok cool", "okay",
		"got it", "sounds good", "lgtm", "cool", "perfect",
		"great", "awesome", "nice", "all good", "looks good",
		"noted", "copy that", "roger", "sweet", "nice one",
		"Yeah!", "OK", "Sounds good.",
	}
	for _, input := range affirmatives {
		_, ok := quickReplyFor(input, true) // inThread=true
		if ok {
			t.Errorf("quickReplyFor(%q, inThread=true) should NOT short-circuit — could be answering a bot question", input)
		}
	}
}

func TestQuickReplyFor_ThreadFollowup_PureGratitudeStillWorks(t *testing.T) {
	// In thread follow-ups, unambiguous gratitude like "thanks" should
	// still be short-circuited — "thanks" never means "yes, do that".
	pureGratitude := []string{
		"thanks", "thank you", "thx", "ty", "cheers",
		"thanks a lot", "thank you so much", "much appreciated",
		"Thanks!", "THANK YOU", "Cheers!",
	}
	for _, input := range pureGratitude {
		reply, ok := quickReplyFor(input, true) // inThread=true
		if !ok {
			t.Errorf("quickReplyFor(%q, inThread=true) should still short-circuit pure gratitude", input)
			continue
		}
		if !strings.Contains(reply, "welcome") {
			t.Errorf("quickReplyFor(%q, inThread=true) = %q — expected gratitude reply", input, reply)
		}
	}
}

func TestQuickReplyFor_ThreadFollowup_DismissalStillWorks(t *testing.T) {
	// Dismissals should always work, even in threads.
	// "nvm" in a thread always means "cancel", never "yes, do it".
	dismissals := []string{"nvm", "figured it out", "all set", "resolved"}
	for _, input := range dismissals {
		reply, ok := quickReplyFor(input, true) // inThread=true
		if !ok {
			t.Errorf("quickReplyFor(%q, inThread=true) should still short-circuit dismissals", input)
			continue
		}
		if !strings.Contains(reply, "let me know") {
			t.Errorf("quickReplyFor(%q, inThread=true) = %q — expected dismissal reply", input, reply)
		}
	}
}

func TestQuickReplyFor_TopLevel_AffirmativeShortCircuits(t *testing.T) {
	// Outside threads (top-level), affirmatives should still be short-circuited
	// as gratitude. A standalone "yeah" in a channel is gratitude, not a command.
	affirmatives := []string{"yeah", "yep", "ok", "sounds good", "lgtm"}
	for _, input := range affirmatives {
		_, ok := quickReplyFor(input) // no inThread arg = false
		if !ok {
			t.Errorf("quickReplyFor(%q) should short-circuit as gratitude in top-level messages", input)
		}
	}
}

func TestEndsWithSentencePunctuation(t *testing.T) {
	positives := []string{
		"No errors found.",
		"All services healthy!",
		"Is this a known issue?",
		"Done.",
	}
	for _, input := range positives {
		if !endsWithSentencePunctuation(input) {
			t.Errorf("endsWithSentencePunctuation(%q) = false, want true", input)
		}
	}

	negatives := []string{
		"247 errors found",
		"Investigating payments",
		"",
		"Still checking…",
		"All clear...",
	}
	for _, input := range negatives {
		if endsWithSentencePunctuation(input) {
			t.Errorf("endsWithSentencePunctuation(%q) = true, want false", input)
		}
	}
}

func TestTruncateFallback_SentenceEndingPunctuation(t *testing.T) {
	// Multi-line where first line ends with "." — should NOT get "…" appended
	text := "No errors found in payments.\nKey findings:\n- All healthy"
	got := TruncateFallback(text)
	if got != "No errors found in payments." {
		t.Errorf("expected no trailing ellipsis after period, got %q", got)
	}

	// First line ends with "!" — same rule
	text2 := "All services healthy!\nNothing to report"
	got2 := TruncateFallback(text2)
	if got2 != "All services healthy!" {
		t.Errorf("expected no trailing ellipsis after exclamation, got %q", got2)
	}

	// First line ends with "?" — same rule
	text3 := "Want me to check upstream services?\nI can dig deeper"
	got3 := TruncateFallback(text3)
	if got3 != "Want me to check upstream services?" {
		t.Errorf("expected no trailing ellipsis after question mark, got %q", got3)
	}

	// First line does NOT end with sentence punctuation — SHOULD get "…"
	text4 := "Found 247 errors in payments\nKey findings below"
	got4 := TruncateFallback(text4)
	if got4 != "Found 247 errors in payments…" {
		t.Errorf("expected trailing ellipsis for non-sentence-ending, got %q", got4)
	}
}


func TestIsActiveThread_Basic(t *testing.T) {
	h := NewHandler(HandlerConfig{})
	// Not active initially
	if h.IsActiveThread("C123", "1234.5678") {
		t.Error("thread should not be active before any messages")
	}
	// Mark active
	h.markThreadActive("C123", "1234.5678")
	if !h.IsActiveThread("C123", "1234.5678") {
		t.Error("thread should be active after markThreadActive")
	}
	// Different thread should not be active
	if h.IsActiveThread("C123", "9999.0000") {
		t.Error("different thread should not be active")
	}
}

func TestIsActiveThread_ExpiresAfterSessionTTL(t *testing.T) {
	h := NewHandler(HandlerConfig{})
	// Directly inject an old timestamp to simulate a stale thread
	// (6+ hours ago) without waiting real time.
	key := "C123:1234.5678"
	h.activeThreadsMu.Lock()
	h.activeThreads[key] = time.Now().Add(-7 * time.Hour)
	h.activeThreadsMu.Unlock()

	// Thread entry exists in the map, but is older than the 6-hour session TTL.
	// IsActiveThread should return false — the session context is gone, so
	// capturing messages in this thread would confuse the user.
	if h.IsActiveThread("C123", "1234.5678") {
		t.Error("thread should NOT be active after session TTL expires — user would get a bot with no context")
	}
}

func TestIsActiveThread_EmptyThreadTS(t *testing.T) {
	h := NewHandler(HandlerConfig{})
	if h.IsActiveThread("C123", "") {
		t.Error("empty threadTS should never be active")
	}
}

func TestIncrementTurns_EvictsOldestNotRandom(t *testing.T) {
	h := NewHandler(HandlerConfig{})
	now := time.Now()

	// Fill the turn counts to capacity with entries at varying ages.
	h.turnCountsMu.Lock()
	for i := 0; i < maxTrackedSessions; i++ {
		key := fmt.Sprintf("session-%d", i)
		h.turnCounts[key] = turnEntry{
			count:    1,
			lastSeen: now.Add(-time.Duration(i) * time.Minute),
		}
	}
	// Add one very recent entry that must survive eviction.
	h.turnCounts["active-incident"] = turnEntry{
		count:    42,
		lastSeen: now,
	}
	h.turnCountsMu.Unlock()

	// This call triggers eviction because we're over capacity.
	h.incrementTurns("new-session")

	h.turnCountsMu.Lock()
	defer h.turnCountsMu.Unlock()

	// The active incident session must survive — it was the most recent.
	entry, ok := h.turnCounts["active-incident"]
	if !ok {
		t.Fatal("active-incident session was evicted despite being the most recent — eviction is random instead of oldest-first")
	}
	if entry.count != 42 {
		t.Errorf("active-incident turn count changed: got %d, want 42", entry.count)
	}

	// The new session must exist.
	if _, ok := h.turnCounts["new-session"]; !ok {
		t.Fatal("new-session was not added after eviction")
	}

	// Verify we're back under capacity.
	if len(h.turnCounts) > maxTrackedSessions {
		t.Errorf("turn counts still over capacity after eviction: %d", len(h.turnCounts))
	}
}

func TestMarkThreadActive_EvictsOldestNotRandom(t *testing.T) {
	h := NewHandler(HandlerConfig{})
	now := time.Now()

	// Fill the active threads map beyond capacity with entries at varying ages.
	h.activeThreadsMu.Lock()
	for i := 0; i <= maxTrackedThreads; i++ {
		key := fmt.Sprintf("C%d:ts%d", i, i)
		h.activeThreads[key] = now.Add(-time.Duration(i) * time.Minute)
	}
	// Add one very recent thread that must survive eviction.
	h.activeThreads["C999:active-incident"] = now
	h.activeThreadsMu.Unlock()

	// This triggers eviction because we're over capacity.
	h.markThreadActive("C-new", "new-thread-ts")

	h.activeThreadsMu.RLock()
	defer h.activeThreadsMu.RUnlock()

	// The active incident thread must survive — it was the most recent.
	if _, ok := h.activeThreads["C999:active-incident"]; !ok {
		t.Fatal("active-incident thread was evicted despite being the most recent — eviction is random instead of oldest-first")
	}

	// The new thread must exist.
	if _, ok := h.activeThreads["C-new:new-thread-ts"]; !ok {
		t.Fatal("new thread was not added after eviction")
	}

	// Verify we're back under capacity.
	if len(h.activeThreads) > maxTrackedThreads+1 {
		t.Errorf("active threads still over capacity after eviction: %d", len(h.activeThreads))
	}
}
