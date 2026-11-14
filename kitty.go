package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// kittySupported reports whether the current terminal understands the
// kitty graphics protocol (kitty itself, or terminals that set the same
// env vars, e.g. Ghostty/WezTerm in kitty-compat mode).
func kittySupported() bool {
	if os.Getenv("KITTY_WINDOW_ID") != "" {
		return true
	}
	return strings.Contains(os.Getenv("TERM"), "kitty")
}

// kittyDeleteImage removes any previously placed image under the given ID
// so repeated refreshes don't leak image data in the terminal's memory.
func kittyDeleteImage(id int) string {
	return fmt.Sprintf("\x1b_Ga=d,d=i,i=%d\x1b\\", id)
}

// kittyShowImage emits the escape sequence(s) to transmit+display a PNG at
// the current cursor position, scaled to fill cols x rows terminal cells.
// f=100 is PNG; the protocol requires chunking base64 payloads over 4096
// bytes using m=1 (more chunks follow) / m=0 (last chunk).
func kittyShowImage(png []byte, id, cols, rows int) string {
	b64 := base64.StdEncoding.EncodeToString(png)
	const chunkSize = 4096
	var b strings.Builder
	for i := 0; i < len(b64); i += chunkSize {
		end := min(i+chunkSize, len(b64))
		more := 0
		if end < len(b64) {
			more = 1
		}
		if i == 0 {
			fmt.Fprintf(&b, "\x1b_Ga=T,f=100,i=%d,c=%d,r=%d,m=%d;%s\x1b\\", id, cols, rows, more, b64[i:end])
		} else {
			fmt.Fprintf(&b, "\x1b_Gm=%d;%s\x1b\\", more, b64[i:end])
		}
	}
	return b.String()
}
