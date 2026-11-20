package main

import (
	"strings"
	"testing"
)

// typedstream builds the part of an attributedBody blob the decoder
// reads: an NSString class marker, then '+' and a length-prefixed string.
func typedstream(text string) []byte {
	b := []byte("\x04\x0bstreamtyped\x81\xe8\x03\x84\x01@\x84\x84\x84\x12NSAttributedString\x00\x84\x84\x08NSObject\x00\x85\x92\x84\x84\x84\x08NSString\x01\x94\x84\x01+")
	n := len(text)
	switch {
	case n < 0x80:
		b = append(b, byte(n))
	default:
		b = append(b, 0x81, byte(n), byte(n>>8))
	}
	b = append(b, text...)
	return append(b, "\x86\x84\x02iI\x01"...)
}

func TestDecodeAttributedBody(t *testing.T) {
	for _, text := range []string{"hi", "émoji 🙏 ok", strings.Repeat("long ", 60)} {
		if got := decodeAttributedBody(typedstream(text)); got != text {
			t.Errorf("decode(%d bytes) = %q, want %q", len(text), got, text)
		}
	}
	if got := decodeAttributedBody([]byte("garbage")); got != "" {
		t.Errorf("garbage decoded to %q", got)
	}
	if got := decodeAttributedBody([]byte("NSString\x01+\x81\xff")); got != "" {
		t.Errorf("truncated blob decoded to %q", got)
	}
}

func TestContactKey(t *testing.T) {
	if contactKey("+62 812-3456-7890") != contactKey("0812 3456 7890") {
		t.Error("international and local forms of one number should match")
	}
	if contactKey("Someone@Example.com ") != "someone@example.com" {
		t.Error("emails should be trimmed and lowercased")
	}
}
