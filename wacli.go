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

// Muted reports whether the chat is currently muted: wacli uses -1 for a
// forever mute and a future unix-seconds timestamp for a timed one (0, or a
// past timestamp from an expired mute, means not muted).
func (c Chat) Muted() bool {
	return c.MutedUntil == -1 || c.MutedUntil > time.Now().Unix()
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
	Text         string `json:"Text"` // the sender's own text, without the "> quoted" header DisplayText adds to replies
	DisplayText  string `json:"DisplayText"`
	MediaType    string `json:"MediaType"`
	MediaCaption string `json:"MediaCaption"`
	Filename     string `json:"Filename"` // original filename, set for document sends; "" for image/video/sticker
	MimeType     string `json:"MimeType"` // e.g. "image/jpeg"; used to pick a sane extension when saving media without a Filename
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

// wacliBackend is the WhatsApp Backend, driving the wacli CLI.
type wacliBackend struct{}

func (wacliBackend) Name() string { return "WhatsApp" }

func (wacliBackend) Caps() Caps {
	return Caps{Reply: true, React: true, Delete: true, MarkRead: true, Mentions: true, Stickers: true}
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

func (wacliBackend) FetchChats() ([]Chat, error) {
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
func (wacliBackend) FetchMessages(chatJID string, limit int) ([]Message, error) {
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

// writeLockWait is passed as --lock-wait on `messages delete`, which has no
// delegate-over-socket fallback in wacli: it must actually wait its turn for
// the store lock (e.g. racing the periodic reload), so a long wait is the
// only way for it to succeed while a transient writer holds the lock.
// wacli's own default for --lock-wait is 0, i.e. no wait.
const writeLockWait = "5s"

// delegatedLockWait is passed on send/react/mark-read, which all delegate
// over wacli's send socket when the lock is held — normally by `wacli sync
// --follow`, which holds it for its entire run. Waiting out writeLockWait's
// 5s for a lock that a running sync will never release just taxes every
// send; a short wait still lets a same-process race (e.g. two quick sends
// issued back to back with no sync daemon running) resolve on its own
// before falling back to delegation.
const delegatedLockWait = "500ms"

// markChatRead marks chatJID as read, sending real read receipts for its
// synced unread messages (per wacli's own read-receipt privacy setting).
func (wacliBackend) MarkChatRead(chatJID string) error {
	var env envelope[any]
	if err := runWacli(&env, "chats", "mark-read", "--chat", chatJID, "--receipts", "--lock-wait", delegatedLockWait); err != nil {
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
// through so WhatsApp actually tags and notifies them. --allow-self is
// always passed: wacli otherwise refuses a text send to the linked
// account's own chat (its "Message Yourself" delivery ack is unreliable,
// but wtui has no separate chat kind to special-case here).
func (wacliBackend) SendText(chatJID, text, replyTo string, mentions []string) error {
	args := []string{"send", "text", "--to", chatJID, "--message", text, "--lock-wait", delegatedLockWait, "--allow-self"}
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
func (wacliBackend) SendFile(chatJID, path, caption, replyTo string) error {
	args := []string{"send", "file", "--to", chatJID, "--file", path, "--lock-wait", delegatedLockWait}
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

// sendSticker sends path (a WebP image) to chatJID as a sticker. Unlike
// sendFile, wacli's sticker send has no --caption flag — WhatsApp stickers
// can't carry one.
func (wacliBackend) SendSticker(chatJID, path, replyTo string) error {
	args := []string{"send", "sticker", "--to", chatJID, "--file", path, "--lock-wait", delegatedLockWait}
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
func (wacliBackend) SendReact(chatJID, msgID, senderJID, emoji string) error {
	args := []string{"send", "react", "--to", chatJID, "--id", msgID, "--reaction", emoji, "--lock-wait", delegatedLockWait}
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
func (wacliBackend) DeleteMessage(chatJID, msgID string, forMe bool) error {
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
func (wacliBackend) ResolveMentionNames(ids []string) (map[string]string, error) {
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

// fetchOwnMentionIDs returns the signed-in account's own @mention digits:
// its phone number (from "wacli doctor"'s linked_jid) and, separately, its
// LID, since a mention token embeds whichever of the two the sender's
// client happened to use. The LID lookup is best-effort — if it fails, the
// phone number alone is still returned, since that's enough to catch most
// mentions.
func (wacliBackend) FetchOwnMentionIDs() (map[string]bool, error) {
	var env envelope[struct {
		LinkedJID string `json:"linked_jid"`
	}]
	if err := runWacli(&env, "doctor"); err != nil {
		return nil, err
	}
	if !env.Success {
		return nil, fmt.Errorf("wacli: %s", deref(env.Error))
	}
	phone, _, ok := strings.Cut(env.Data.LinkedJID, "@")
	if !ok || phone == "" {
		return nil, fmt.Errorf("wacli doctor: no linked_jid")
	}
	ids := map[string]bool{phone: true}

	var lidEnv envelope[[]mentionResolution]
	if err := runWacli(&lidEnv, "contacts", "resolve", phone); err == nil && lidEnv.Success && len(lidEnv.Data) > 0 {
		if lid := strings.TrimSuffix(lidEnv.Data[0].LID, "@lid"); lid != "" {
			ids[lid] = true
		}
	}
	return ids, nil
}

func deref(s *string) string {
	if s == nil {
		return "unknown error"
	}
	return *s
}
