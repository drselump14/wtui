package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// clipboardImage saves the system clipboard's image, if any, to a new temp
// PNG file and returns its path. The caller removes the file once wacli no
// longer needs it (on send success), or keeps it around for a retry.
func clipboardImage() (string, error) {
	if _, err := exec.LookPath("pngpaste"); err != nil {
		return "", fmt.Errorf("pasting images needs pngpaste (brew install pngpaste)")
	}
	f, err := os.CreateTemp("", "wtui-paste-*.png")
	if err != nil {
		return "", err
	}
	path := f.Name()
	f.Close()

	if out, err := exec.Command("pngpaste", path).CombinedOutput(); err != nil {
		os.Remove(path)
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}
	return path, nil
}
