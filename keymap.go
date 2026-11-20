package main

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
)

// appKeys is the active keymap, loaded once in main() before the Bubble
// Tea program starts. nil only in tests that never call loadKeymap; action
// tolerates that by returning "" (no binding), same as an unmapped key.
var appKeys *keymap

// binding is one action's default key(s) in a mode. mode names match the
// modal states Update's key handling dispatches on: "global" is checked in
// every mode, "browse" is the default chat/message view, and the rest each
// correspond to one of its popups or prompts (see keyblock in Update).
type binding struct {
	mode   string
	action string
	keys   []string
}

// defaultBindings is wtui's built-in keymap. Every entry here can be
// overridden — replacing that action's whole key list — via
// keymapPath (see loadKeymap). Two gestures stay fixed: ctrl+c always
// quits, even if "global.quit" is overridden without it (Update checks it
// before the keymap), so a config typo can never leave wtui unquittable;
// and "g g" (press g twice) jumps to the top, since Update's key handling
// only resolves single keypresses, not chords. Extra quit keys that work
// in every mode, including while composing, go in "global":
// { "global": { "quit": ["ctrl+x"] } }.
var defaultBindings = []binding{
	{"global", "quit", []string{"ctrl+c"}},

	{"browse", "quit", []string{"q"}},
	{"browse", "bottom", []string{"G"}},
	{"browse", "halfPageDown", []string{"ctrl+d"}},
	{"browse", "halfPageUp", []string{"ctrl+u"}},
	{"browse", "up", []string{"up"}},
	{"browse", "vimUp", []string{"k"}},
	{"browse", "down", []string{"down"}},
	{"browse", "vimDown", []string{"j"}},
	{"browse", "pageUp", []string{"pgup"}},
	{"browse", "pageDown", []string{"pgdown"}},
	{"browse", "searchMessages", []string{"/"}},
	{"browse", "searchChats", []string{"ctrl+g", "ctrl+/", "ctrl+_"}},
	{"browse", "open", []string{"enter", "l", "right"}},
	{"browse", "openMedia", []string{"o"}},
	{"browse", "saveMedia", []string{"s"}},
	{"browse", "compose", []string{"i"}},
	{"browse", "markChatRead", []string{"A"}},
	{"browse", "markMutedRead", []string{"M"}},
	{"browse", "reply", []string{"r"}},
	{"browse", "react", []string{"e"}},
	{"browse", "delete", []string{"d"}},
	{"browse", "copyText", []string{"y"}},
	{"browse", "copyID", []string{"Y"}},
	{"browse", "showReactors", []string{"E"}},
	{"browse", "retrySend", []string{"R"}},
	{"browse", "dismissFailedSend", []string{"x"}},
	{"browse", "jumpToQuoted", []string{"f"}},
	{"browse", "switchChat", []string{"ctrl+n", "ctrl+p"}},
	{"browse", "nextMatch", []string{"n"}},
	{"browse", "prevMatch", []string{"p", "N"}},
	{"browse", "clearFilter", []string{"h", "left", "esc"}},
	{"browse", "help", []string{"?"}},

	{"reactors", "prevTab", []string{"h", "left"}},
	{"reactors", "nextTab", []string{"l", "right"}},
	{"reactors", "prevName", []string{"k", "up"}},
	{"reactors", "nextName", []string{"j", "down"}},

	{"attach", "cancel", []string{"esc"}},
	{"attach", "confirm", []string{"enter"}},
	{"attach", "complete", []string{"tab"}},

	{"compose", "cancel", []string{"esc"}},
	{"compose", "vertical", []string{"up", "down"}},
	{"compose", "switchChat", []string{"ctrl+n", "ctrl+p"}},
	{"compose", "openEditor", []string{"ctrl+g"}},
	{"compose", "pasteImage", []string{"ctrl+v"}},
	{"compose", "attachFile", []string{"ctrl+t"}},
	// enter is a plain new line (and confirms IME conversions without
	// sending); shift+enter sends. alt+enter sends too, for terminals
	// (e.g. a kitty.conf "map shift+enter send_text all \e\r") that turn
	// shift+enter into alt+enter before wtui sees it.
	{"compose", "send", []string{"shift+enter", "alt+enter"}},
	{"compose", "newline", []string{"enter", "ctrl+j"}},
	{"compose", "acceptMention", []string{"tab"}},

	{"search", "close", []string{"esc", "ctrl+g", "ctrl+/", "ctrl+_"}},
	{"search", "commit", []string{"enter"}},
	{"search", "prev", []string{"up", "ctrl+p"}},
	{"search", "next", []string{"down", "ctrl+n"}},

	{"msgsearch", "close", []string{"esc"}},
	{"msgsearch", "commit", []string{"enter"}},
	{"msgsearch", "next", []string{"ctrl+n", "down"}},
	{"msgsearch", "prev", []string{"ctrl+p", "up"}},

	{"delconfirm", "cancel", []string{"esc", "n"}},
	{"delconfirm", "forMe", []string{"1", "y"}},
	{"delconfirm", "everyone", []string{"2"}},

	{"react", "cancel", []string{"esc"}},
	{"react", "prev", []string{"left", "h"}},
	{"react", "next", []string{"right", "l"}},
	{"react", "up", []string{"up", "k"}},
	{"react", "down", []string{"down", "j"}},
	{"react", "confirm", []string{"enter"}},
}

// keymap resolves a (mode, pressed key) pair to the action bound to it,
// after merging ~/.wtui.keys.json's overrides onto defaultBindings.
type keymap struct {
	byKey map[string]map[string]string // mode -> key -> action
}

// action returns the action bound to key in mode, or "" if key has no
// binding there — callers treat "" as "not one of my named actions" and
// fall to their switch's default case (usually forwarding the key to a
// text input).
func (k *keymap) action(mode, key string) string {
	if k == nil {
		return ""
	}
	return k.byKey[mode][key]
}

// keymapPath is where a user overrides wtui's keybindings: $XDG_CONFIG_HOME
// (default ~/.config) + "wtui/keys.json", the standard per-app config
// location rather than a bare dotfile.
func keymapPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "wtui", "keys.json")
}

// loadKeymap builds the active keymap from defaultBindings, with any
// per-action overrides from keymapPath applied on top: an override
// replaces that action's whole key list; an action the file doesn't
// mention keeps its default. A missing file is normal and silent; a
// present-but-invalid one is logged to appLog and otherwise ignored, so a
// typo in the config degrades to defaults rather than crashing wtui.
//
// Config shape (only the actions you want to change):
//
//	{
//	  "browse": { "markMutedRead": ["ctrl+m"], "quit": ["Q"] },
//	  "compose": { "send": ["ctrl+enter"] }
//	}
func loadKeymap() *keymap {
	merged := map[string]map[string][]string{} // mode -> action -> keys
	for _, b := range defaultBindings {
		if merged[b.mode] == nil {
			merged[b.mode] = map[string][]string{}
		}
		merged[b.mode][b.action] = b.keys
	}

	path := keymapPath()
	if data, err := os.ReadFile(path); err == nil {
		var overrides map[string]map[string][]string
		if err := json.Unmarshal(data, &overrides); err != nil {
			if appLog != nil {
				appLog.Printf("keymap: ignoring invalid %s: %v", path, err)
			}
		} else {
			for mode, actions := range overrides {
				if merged[mode] == nil {
					merged[mode] = map[string][]string{}
				}
				maps.Copy(merged[mode], actions)
			}
		}
	} else if !os.IsNotExist(err) && appLog != nil {
		appLog.Printf("keymap: reading %s: %v", path, err)
	}

	byKey := map[string]map[string]string{}
	for mode, actions := range merged {
		byKey[mode] = map[string]string{}
		for action, keys := range actions {
			for _, key := range keys {
				byKey[mode][key] = action
			}
		}
	}
	return &keymap{byKey: byKey}
}
