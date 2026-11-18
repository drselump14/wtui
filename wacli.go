package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Chat mirrors a row from `wacli chats list --json`.
type Chat struct {
	JID           string `json:"jid"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	LastMessageTS string `json:"last_message_ts"`
	Archived      bool   `json:"archived"`
	Pinned        bool   `json:"pinned"`
	MutedUntil    int64  `json:"muted_until"`
	Unread        bool   `json:"unread"`
	UnreadCount   int    `json:"unread_count"`
}

// Message mirrors a row from `wacli messages list --json`.
type Message struct {
	ChatJID      string `json:"ChatJID"`
	ChatName     string `json:"ChatName"`
	MsgID        string `json:"MsgID"`
	SenderJID    string `json:"SenderJID"`
	SenderName   string `json:"SenderName"`
	Timestamp    string `json:"Timestamp"`
	FromMe       bool   `json:"FromMe"`
	DisplayText  string `json:"DisplayText"`
	MediaType    string `json:"MediaType"`
	MediaCaption string `json:"MediaCaption"`
	Revoked      bool   `json:"Revoked"`
	DeletedForMe bool   `json:"DeletedForMe"`
	QuotedMsgID  string `json:"quoted_msg_id"`

	// ReactionToID and ReactionEmoji are set only on a reaction row (a
	// message of its own in wacli's store): ReactionToID is the MsgID being
	// reacted to and ReactionEmoji the emoji ("" for a removed reaction).
	// wtui never shows these as messages — it folds them into a badge under
	// the target message instead (see loadMessages).
	ReactionToID  string `json:"ReactionToID"`
	ReactionEmoji string `json:"ReactionEmoji"`
}

type envelope[T any] struct {
	Success bool    `json:"success"`
	Data    T       `json:"data"`
	Error   *string `json:"error"`
}

type messagesPage struct {
	Messages []Message `json:"messages"`
}

func runWacli(out any, args ...string) error {
	full := append([]string{"--json"}, args...)
	start := time.Now()
	cmd := exec.Command("wacli", full...)
	stdout, err := cmd.Output()
	dur := time.Since(start).Round(time.Millisecond)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			stderr := strings.TrimSpace(string(ee.Stderr))
			if appLog != nil {
				appLog.Printf("wacli %v FAILED (%s): %s", args, dur, stderr)
			}
			if strings.Contains(stderr, "store is locked") || strings.Contains(stderr, "store lock") {
				// messages delete/revoke and chats mark-read contend
				// directly for the connection lock, unlike `send *`, which
				// a running `sync --follow` delegates instead of blocking.
				return fmt.Errorf("wacli %v: can't reach the store — wacli sync --follow is running and holds it; stop it, retry, then restart sync (%s)", args, stderr)
			}
			return fmt.Errorf("wacli %v: %s", args, stderr)
		}
		if appLog != nil {
			appLog.Printf("wacli %v FAILED (%s): %v (is wacli on PATH?)", args, dur, err)
		}
		return fmt.Errorf("wacli %v: %w (is wacli on PATH?)", args, err)
	}
	if appLog != nil {
		appLog.Printf("wacli %v ok (%s)", args, dur)
	}
	return json.Unmarshal(stdout, out)
}

func fetchChats() ([]Chat, error) {
	var env envelope[[]Chat]
	if err := runWacli(&env, "chats", "list", "--no-archived", "--limit", "200"); err != nil {
		return nil, err
	}
	if !env.Success {
		return nil, fmt.Errorf("wacli: %s", deref(env.Error))
	}

	chats := make([]Chat, 0, len(env.Data))
	for _, c := range env.Data {
		if (c.Kind == "newsletter" || c.Kind == "channel") && c.Name != "WhatsApp" {
			continue // drop broadcast channels other than the official WhatsApp Channel
		}
		chats = append(chats, c)
	}
	return chats, nil
}

// fetchMessages returns the latest `limit` messages for chatJID, oldest first
// (ready to render top-to-bottom). wacli's default sort is newest-first, so
// we fetch that way (to get the *latest* N) then reverse for display.
func fetchMessages(chatJID string, limit int) ([]Message, error) {
	var env envelope[messagesPage]
	if err := runWacli(&env, "messages", "list", "--chat", chatJID, "--limit", fmt.Sprint(limit)); err != nil {
		return nil, err
	}
	if !env.Success {
		return nil, fmt.Errorf("wacli: %s", deref(env.Error))
	}
	msgs := env.Data.Messages
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// writeLockWait is passed as --lock-wait on every write command (send,
// react, delete, mark-read) so a second one issued while another is still
// holding the store lock (e.g. a reaction fired while a file send is
// writing, or a delete racing the periodic reload) waits its turn instead
// of failing immediately with a lock error — wacli's own default for
// --lock-wait is 0, i.e. no wait.
const writeLockWait = "5s"

// markChatRead marks chatJID as read, sending real read receipts for its
// synced unread messages (per wacli's own read-receipt privacy setting).
func markChatRead(chatJID string) error {
	var env envelope[any]
	if err := runWacli(&env, "chats", "mark-read", "--chat", chatJID, "--receipts", "--lock-wait", writeLockWait); err != nil {
		return err
	}
	if !env.Success {
		return fmt.Errorf("wacli: %s", deref(env.Error))
	}
	return nil
}

// sendText sends a real WhatsApp text message to chatJID. If replyTo is
// non-empty, the message is sent as a quoted reply to that MsgID. mentions
// is the phone numbers embedded as "@<phone>" tokens in text, passed
// through so WhatsApp actually tags and notifies them.
func sendText(chatJID, text, replyTo string, mentions []string) error {
	args := []string{"send", "text", "--to", chatJID, "--message", text, "--lock-wait", writeLockWait}
	if replyTo != "" {
		args = append(args, "--reply-to", replyTo)
	}
	for _, phone := range mentions {
		args = append(args, "--mention", phone)
	}
	var env envelope[any]
	if err := runWacli(&env, args...); err != nil {
		return err
	}
	if !env.Success {
		return fmt.Errorf("wacli: %s", deref(env.Error))
	}
	return nil
}

// sendFile sends a file (image/video/audio/document) to chatJID, detected
// and typed automatically by wacli. If caption is non-empty it's attached
// as the media's caption; if replyTo is non-empty, the message is sent as
// a quoted reply to that MsgID.
func sendFile(chatJID, path, caption, replyTo string) error {
	args := []string{"send", "file", "--to", chatJID, "--file", path, "--lock-wait", writeLockWait}
	if caption != "" {
		args = append(args, "--caption", caption)
	}
	if replyTo != "" {
		args = append(args, "--reply-to", replyTo)
	}
	var env envelope[any]
	if err := runWacli(&env, args...); err != nil {
		return err
	}
	if !env.Success {
		return fmt.Errorf("wacli: %s", deref(env.Error))
	}
	return nil
}

// sendReact sends (or, with emoji == "", removes) a reaction to msgID in
// chatJID. senderJID is the target message's original sender and is
// required by wacli for group chats; it's ignored for 1:1 chats.
func sendReact(chatJID, msgID, senderJID, emoji string) error {
	args := []string{"send", "react", "--to", chatJID, "--id", msgID, "--reaction", emoji, "--lock-wait", writeLockWait}
	if senderJID != "" {
		args = append(args, "--sender", senderJID)
	}
	var env envelope[any]
	if err := runWacli(&env, args...); err != nil {
		return err
	}
	if !env.Success {
		return fmt.Errorf("wacli: %s", deref(env.Error))
	}
	return nil
}

// deleteMessage removes msgID in chatJID: for everyone (wacli's "delete"
// without --for-me, which only works for a message FromMe) or, with forMe
// true, just from this account's own local view (any message, revoked or
// not, is removed locally either way).
func deleteMessage(chatJID, msgID string, forMe bool) error {
	args := []string{"messages", "delete", "--chat", chatJID, "--id", msgID, "--lock-wait", writeLockWait}
	if forMe {
		args = append(args, "--for-me")
	}
	var env envelope[any]
	if err := runWacli(&env, args...); err != nil {
		return err
	}
	if !env.Success {
		return fmt.Errorf("wacli: %s", deref(env.Error))
	}
	return nil
}

type mentionResolution struct {
	Input    string `json:"input"`
	LID      string `json:"lid"`
	Name     string `json:"name"`
	Resolved bool   `json:"resolved"`
}

// resolveMentionNames maps each @mention ID in ids (bare digits, either a
// LID or an older-style phone number — the two are indistinguishable from
// the digits alone) to its contact's saved name, via wacli's local LID
// map. wacli's "contacts resolve" needs an explicit "@lid" suffix to try
// the LID interpretation of a bare digit string (it otherwise assumes
// phone), so each id is tried both ways and whichever resolves wins. IDs
// wacli can't resolve under either interpretation, or resolves but has no
// saved name for, are simply omitted from the result — the caller falls
// back to showing the raw ID for those.
func resolveMentionNames(ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]string, 0, len(ids)*2+2)
	args = append(args, "contacts", "resolve")
	for _, id := range ids {
		args = append(args, id, id+"@lid")
	}
	var env envelope[[]mentionResolution]
	if err := runWacli(&env, args...); err != nil {
		return nil, err
	}
	if !env.Success {
		return nil, fmt.Errorf("wacli: %s", deref(env.Error))
	}
	names := map[string]string{}
	for _, r := range env.Data {
		if r.Name == "" {
			continue
		}
		names[strings.TrimSuffix(r.Input, "@lid")] = r.Name
	}
	return names, nil
}

func deref(s *string) string {
	if s == nil {
		return "unknown error"
	}
	return *s
}
