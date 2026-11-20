package main

import "testing"

func TestSplitQuotedReply(t *testing.T) {
	tests := []struct {
		name, text, own, quote, body string
	}{
		{"not a reply", "hello", "hello", "", "hello"},
		{"single-line quote", "> hi\nreply", "reply", "hi", "reply"},
		{"multi-line quote", "> https://example.com/x\n\nwow\n\nmore ...\nmy reply", "my reply", "https://example.com/x\n\nwow\n\nmore ...", "my reply"},
		{"multi-line quote and body", "> a\nb\nline1\nline2", "line1\nline2", "a\nb", "line1\nline2"},
		{"own text unknown falls back", "> a\nb", "", "a", "b"},
		{"own text mismatch falls back", "> a\nb\nc", "zzz", "a", "b\nc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, b := splitQuotedReply(tt.text, tt.own)
			if q != tt.quote || b != tt.body {
				t.Errorf("splitQuotedReply(%q, %q) = (%q, %q), want (%q, %q)", tt.text, tt.own, q, b, tt.quote, tt.body)
			}
		})
	}
}

func TestCaptionAwareTextMultiLineQuote(t *testing.T) {
	msg := Message{
		DisplayText:  "> line one\nline two\nSent image",
		Text:         "Sent image",
		MediaType:    "image",
		MediaCaption: "look",
	}
	if got, want := captionAwareText(msg), "> line one\nline two\nlook"; got != want {
		t.Errorf("captionAwareText = %q, want %q", got, want)
	}
}

// Caption-less media replies have empty Text; the quote must still end
// right before wacli's "Sent <type>" line.
func TestOwnTextCaptionlessMediaReply(t *testing.T) {
	msg := Message{
		DisplayText: "> first line \n\nsecond line\nSent sticker",
		MediaType:   "sticker",
	}
	q, b := splitQuotedReply(captionAwareText(msg), ownText(msg))
	if q != "first line \n\nsecond line" || b != "Sent sticker" {
		t.Errorf("got (%q, %q)", q, b)
	}
}
