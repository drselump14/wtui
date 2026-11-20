package main

import "testing"

// A refresh that reorders the chat list (e.g. after sending, which moves
// that chat to the top) must keep the cursor on the same chat, not the
// same row.
func TestChatsReloadKeepsCursorOnSameChat(t *testing.T) {
	m := initialModel()
	m.chats = []Chat{{JID: "a"}, {JID: "b"}, {JID: "c"}}
	m.cursor = 2 // on "c"
	m.openJID = "c"

	next, _ := m.update(chatsLoadedMsg{chats: []Chat{{JID: "c"}, {JID: "a"}, {JID: "b"}}})
	if got := next.(model).cursor; got != 0 {
		t.Errorf("cursor = %d, want 0 (chat c moved to the top)", got)
	}

	// A chat that disappears falls back to clamping, as before.
	next, _ = next.(model).update(chatsLoadedMsg{chats: []Chat{{JID: "a"}}})
	if got := next.(model).cursor; got != 0 {
		t.Errorf("cursor = %d, want 0 after list shrank", got)
	}
}
