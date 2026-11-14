package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
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
	Revoked      bool   `json:"Revoked"`
	DeletedForMe bool   `json:"DeletedForMe"`
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
	cmd := exec.Command("wacli", full...)
	stdout, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("wacli %v: %s", args, string(ee.Stderr))
		}
		return fmt.Errorf("wacli %v: %w (is wacli on PATH?)", args, err)
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

// markChatRead marks chatJID as read, sending real read receipts for its
// synced unread messages (per wacli's own read-receipt privacy setting).
func markChatRead(chatJID string) error {
	var env envelope[any]
	if err := runWacli(&env, "chats", "mark-read", "--chat", chatJID, "--receipts"); err != nil {
		return err
	}
	if !env.Success {
		return fmt.Errorf("wacli: %s", deref(env.Error))
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return "unknown error"
	}
	return *s
}
