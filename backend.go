package main

// Backend is a chat service wtui's UI drives: WhatsApp (via the wacli CLI,
// see wacliBackend) or Apple Messages. IDs are opaque strings the backend
// hands out in Chat.JID / Message.MsgID and gets back unchanged.
type Backend interface {
	// Name is the service's user-facing name, e.g. for notices.
	Name() string
	// Caps reports which optional features the backend supports; the UI
	// shows a notice instead of calling an unsupported method.
	Caps() Caps

	FetchChats() ([]Chat, error)
	// FetchMessages returns chatID's latest limit messages, oldest first.
	FetchMessages(chatID string, limit int) ([]Message, error)
	MarkChatRead(chatID string) error
	SendText(chatID, text, replyTo string, mentions []string) error
	SendFile(chatID, path, caption, replyTo string) error
	SendSticker(chatID, path, replyTo string) error
	SendReact(chatID, msgID, senderID, emoji string) error
	DeleteMessage(chatID, msgID string, forMe bool) error
	ResolveMentionNames(ids []string) (map[string]string, error)
	FetchOwnMentionIDs() (map[string]bool, error)
	// DownloadMedia returns a local path to msgID's media, fetching it
	// first if the backend needs to.
	DownloadMedia(chatID, msgID string) (string, error)
}

// Caps lists optional backend features. Everything not listed here
// (chats, messages, sending text and files, opening media) is required.
type Caps struct {
	Reply    bool // send a message as a quoted reply to another
	React    bool // add/remove a reaction
	Delete   bool // delete a message
	MarkRead bool // mark a chat read
	Mentions bool // @mention autocomplete and tagging
	Stickers bool // send a file as a sticker
}

// be is the active backend, chosen in main() before the UI starts.
var be Backend = wacliBackend{}

// appName is "wtui" (WhatsApp) or "mtui" (Apple Messages), from the name
// the binary runs as or -backend. It names the config directory
// (~/.config/<appName>/keys.toml) and log file (~/.<appName>.log), so the
// two keep separate keybindings.
var appName = "wtui"

// unsupportedAction reports whether a browse-mode action needs a feature
// the active backend lacks, and if so a short name for that feature for
// the "not supported" notice.
func unsupportedAction(action string) (feature string, unsupported bool) {
	c := be.Caps()
	switch action {
	case "reply":
		return "Replying to a message", !c.Reply
	case "react":
		return "Reacting", !c.React
	case "delete":
		return "Deleting messages", !c.Delete
	case "markChatRead", "markMutedRead":
		return "Marking chats read", !c.MarkRead
	}
	return "", false
}
