package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// imessageBackend is the Apple Messages Backend. It reads the Messages
// app's own database (~/Library/Messages/chat.db) read-only through the
// sqlite3 CLI — the terminal needs Full Disk Access for that — and sends
// through AppleScript, which is all macOS exposes for writing: there is no
// way to reply to a specific message, react, delete, or mark read, so
// those Caps are off.
type imessageBackend struct {
	db string // path to chat.db

	contactsOnce sync.Once
	contacts     map[string]string // normalized phone/email -> contact name
}

func newIMessageBackend() *imessageBackend {
	home, _ := os.UserHomeDir()
	return &imessageBackend{db: filepath.Join(home, "Library", "Messages", "chat.db")}
}

func (*imessageBackend) Name() string { return "Messages" }

func (*imessageBackend) Caps() Caps { return Caps{} }

// appleEpoch is chat.db's time origin; its dates are nanoseconds since it.
var appleEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

func appleTime(ns int64) time.Time { return appleEpoch.Add(time.Duration(ns)) }

// sqlQuote renders s as a SQL string literal. Queries are built as text
// because they go through the sqlite3 CLI, which has no bind parameters
// on its command line; every value interpolated goes through this.
func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// query runs sql read-only against db and decodes sqlite3's -json output
// (an array of row objects, or nothing at all for zero rows) into out.
func query(db, sql string, out any) error {
	start := time.Now()
	cmd := exec.Command("sqlite3", "-readonly", "-json", db, sql)
	stdout, err := cmd.Output()
	dur := time.Since(start).Round(time.Millisecond)
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		if appLog != nil {
			appLog.Printf("sqlite3 %s FAILED (%s): %s", filepath.Base(db), dur, msg)
		}
		if strings.Contains(msg, "authorization denied") || strings.Contains(msg, "unable to open") {
			return fmt.Errorf("can't read %s: give your terminal Full Disk Access (System Settings → Privacy & Security) — %s", db, msg)
		}
		return fmt.Errorf("sqlite3 %s: %s", filepath.Base(db), msg)
	}
	if appLog != nil {
		appLog.Printf("sqlite3 %s ok (%s)", filepath.Base(db), dur)
	}
	if len(bytes.TrimSpace(stdout)) == 0 {
		return nil
	}
	return json.Unmarshal(stdout, out)
}

// realMessage filters out rows that aren't user-visible messages: group
// events (renames, joins) and tapbacks, which are folded in separately.
const realMessage = "m.item_type = 0 AND m.associated_message_type = 0"

type imChatRow struct {
	ROWID      int64  `json:"ROWID"`
	GUID       string `json:"guid"`
	Identifier string `json:"chat_identifier"`
	Display    string `json:"display_name"`
	Style      int    `json:"style"`
	Last       int64  `json:"last"`
	Unread     int    `json:"unread"`
	Members    string `json:"members"`
}

func (b *imessageBackend) FetchChats() ([]Chat, error) {
	var rows []imChatRow
	err := query(b.db, `
SELECT c.ROWID, c.guid, c.chat_identifier, c.display_name, c.style,
  (SELECT max(cmj.message_date) FROM chat_message_join cmj WHERE cmj.chat_id = c.ROWID) AS last,
  (SELECT count(*) FROM chat_message_join cmj JOIN message m ON m.ROWID = cmj.message_id
     WHERE cmj.chat_id = c.ROWID AND m.is_read = 0 AND m.is_from_me = 0 AND `+realMessage+`) AS unread,
  (SELECT group_concat(h.id, char(31)) FROM chat_handle_join chj JOIN handle h ON h.ROWID = chj.handle_id
     WHERE chj.chat_id = c.ROWID) AS members
FROM chat c
WHERE c.is_archived = 0 AND last IS NOT NULL
ORDER BY last DESC
LIMIT 200`, &rows)
	if err != nil {
		return nil, err
	}
	chats := make([]Chat, 0, len(rows))
	for _, r := range rows {
		kind := "user"
		if r.Style == 43 { // 43 = group, 45 = one-to-one
			kind = "group"
		}
		chats = append(chats, Chat{
			JID:           r.GUID,
			Kind:          kind,
			Name:          b.chatName(r),
			LastMessageTS: appleTime(r.Last).Format(time.RFC3339),
			Unread:        r.Unread > 0,
			UnreadCount:   r.Unread,
		})
	}
	return chats, nil
}

// chatName is a group's own name, else its members' contact names, else
// the raw phone number / email.
func (b *imessageBackend) chatName(r imChatRow) string {
	if r.Display != "" {
		return r.Display
	}
	var names []string
	for h := range strings.SplitSeq(r.Members, "\x1f") {
		if h != "" {
			names = append(names, b.contactName(h))
		}
	}
	if len(names) == 0 {
		return r.Identifier
	}
	return strings.Join(names, ", ")
}

type imMsgRow struct {
	ROWID        int64  `json:"ROWID"`
	GUID         string `json:"guid"`
	Text         string `json:"text"`
	Body         string `json:"body"` // hex(attributedBody)
	FromMe       int    `json:"is_from_me"`
	Date         int64  `json:"date"`
	Handle       string `json:"handle"`
	AssocType    int    `json:"associated_message_type"`
	AssocGUID    string `json:"associated_message_guid"`
	AssocEmoji   string `json:"associated_message_emoji"`
	ThreadOrigin string `json:"thread_originator_guid"`
	Retracted    int64  `json:"date_retracted"`
	ItemType     int    `json:"item_type"`
}

type imAttRow struct {
	MessageID int64  `json:"message_id"`
	Filename  string `json:"filename"`
	MimeType  string `json:"mime_type"`
	Transfer  string `json:"transfer_name"`
	Sticker   int    `json:"is_sticker"`
}

const imMsgCols = `m.ROWID, m.guid, m.text, hex(m.attributedBody) AS body, m.is_from_me, m.date,
  h.id AS handle, m.associated_message_type, m.associated_message_guid, m.associated_message_emoji,
  m.thread_originator_guid, m.date_retracted, m.item_type`

func (b *imessageBackend) FetchMessages(chatID string, limit int) ([]Message, error) {
	var rows []imMsgRow
	err := query(b.db, fmt.Sprintf(`
SELECT %s
FROM chat c
JOIN chat_message_join cmj ON cmj.chat_id = c.ROWID
JOIN message m ON m.ROWID = cmj.message_id
LEFT JOIN handle h ON h.ROWID = m.handle_id
WHERE c.guid = %s AND m.item_type = 0
ORDER BY cmj.message_date DESC
LIMIT %d`, imMsgCols, sqlQuote(chatID), limit), &rows)
	if err != nil {
		return nil, err
	}
	// Oldest first, as the UI renders top to bottom.
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}

	atts, err := b.attachments(rows)
	if err != nil {
		return nil, err
	}
	quoted, err := b.quotedTexts(rows)
	if err != nil {
		return nil, err
	}

	var msgs []Message
	// Tapbacks: the latest add (2000–2006) or remove (3000–3006) per
	// target message and sender wins; only current ones become rows.
	type tapKey struct{ target, who string }
	taps := map[tapKey]Message{}
	var tapOrder []tapKey
	for _, r := range rows {
		sender, senderName := b.sender(r)
		if r.AssocType >= 2000 && r.AssocType <= 3006 {
			target := r.AssocGUID
			if _, after, ok := strings.Cut(target, "/"); ok {
				target = after // "p:0/<guid>"
			} else {
				target = strings.TrimPrefix(target, "bp:")
			}
			k := tapKey{target, sender}
			if _, seen := taps[k]; !seen {
				tapOrder = append(tapOrder, k)
			}
			emoji := ""
			if r.AssocType < 3000 {
				emoji = tapbackEmoji(r.AssocType, r.AssocEmoji)
			}
			taps[k] = Message{
				ChatJID: chatID, MsgID: r.GUID, SenderJID: sender, SenderName: senderName,
				Timestamp: appleTime(r.Date).Format(time.RFC3339), FromMe: r.FromMe == 1,
				ReactionToID: target, ReactionEmoji: emoji,
			}
			continue
		}
		if r.AssocType != 0 {
			continue
		}

		text := strings.TrimSpace(strings.ReplaceAll(messageText(r), "￼", ""))
		msg := Message{
			ChatJID:    chatID,
			MsgID:      r.GUID,
			SenderJID:  sender,
			SenderName: senderName,
			Timestamp:  appleTime(r.Date).Format(time.RFC3339),
			FromMe:     r.FromMe == 1,
			Text:       text,
			Revoked:    r.Retracted > 0,
		}
		if a, ok := atts[r.ROWID]; ok {
			msg.MediaType = mediaType(a)
			msg.MimeType = a.MimeType
			msg.MediaCaption = text
			if msg.MediaType == "document" {
				msg.Filename = a.Transfer
			}
		}
		msg.DisplayText = text
		if r.ThreadOrigin != "" {
			msg.QuotedMsgID = r.ThreadOrigin
			if q := quoted[r.ThreadOrigin]; q != "" {
				// Same "> quote\nbody" shape wacli uses, which the UI's
				// splitQuotedReply understands.
				own := text
				if own == "" && msg.MediaType != "" {
					own = "Sent " + msg.MediaType
				}
				msg.DisplayText = "> " + q + "\n" + own
			}
		}
		msgs = append(msgs, msg)
	}
	for _, k := range tapOrder {
		if t := taps[k]; t.ReactionEmoji != "" {
			msgs = append(msgs, t)
		}
	}
	return msgs, nil
}

// sender returns a message's sender ID (phone/email, "" for me) and name.
func (b *imessageBackend) sender(r imMsgRow) (id, name string) {
	if r.FromMe == 1 {
		return "", "me"
	}
	return r.Handle, b.contactName(r.Handle)
}

// attachments returns the first attachment of each message in rows that
// has one. The UI shows one media item per message.
func (b *imessageBackend) attachments(rows []imMsgRow) (map[int64]imAttRow, error) {
	var ids []string
	for _, r := range rows {
		ids = append(ids, strconv.FormatInt(r.ROWID, 10))
	}
	out := map[int64]imAttRow{}
	if len(ids) == 0 {
		return out, nil
	}
	var atts []imAttRow
	err := query(b.db, `
SELECT maj.message_id, a.filename, a.mime_type, a.transfer_name, a.is_sticker
FROM message_attachment_join maj JOIN attachment a ON a.ROWID = maj.attachment_id
WHERE maj.message_id IN (`+strings.Join(ids, ",")+`) AND a.hide_attachment = 0
ORDER BY maj.message_id, a.ROWID`, &atts)
	if err != nil {
		return nil, err
	}
	for _, a := range atts {
		if _, ok := out[a.MessageID]; !ok {
			out[a.MessageID] = a
		}
	}
	return out, nil
}

// quotedTexts returns the text of every message that rows reply to, keyed
// by GUID — including ones older than the loaded window.
func (b *imessageBackend) quotedTexts(rows []imMsgRow) (map[string]string, error) {
	var guids []string
	seen := map[string]bool{}
	for _, r := range rows {
		if r.ThreadOrigin != "" && !seen[r.ThreadOrigin] {
			seen[r.ThreadOrigin] = true
			guids = append(guids, sqlQuote(r.ThreadOrigin))
		}
	}
	out := map[string]string{}
	if len(guids) == 0 {
		return out, nil
	}
	var qs []imMsgRow
	err := query(b.db, `SELECT `+imMsgCols+` FROM message m LEFT JOIN handle h ON h.ROWID = m.handle_id
WHERE m.guid IN (`+strings.Join(guids, ",")+`)`, &qs)
	if err != nil {
		return nil, err
	}
	for _, q := range qs {
		t := strings.TrimSpace(strings.ReplaceAll(messageText(q), "￼", ""))
		if t == "" {
			t = "[attachment]"
		}
		out[q.GUID] = t
	}
	return out, nil
}

// messageText is a message's plain text: the text column when set, else
// decoded from attributedBody, where recent macOS versions keep it.
func messageText(r imMsgRow) string {
	if r.Text != "" {
		return r.Text
	}
	if r.Body == "" {
		return ""
	}
	blob, err := hex.DecodeString(r.Body)
	if err != nil {
		return ""
	}
	return decodeAttributedBody(blob)
}

// decodeAttributedBody extracts the string from an NSAttributedString
// archived in Apple's typedstream format (chat.db's attributedBody). It
// doesn't parse the whole stream: the text is the first NSString's
// payload, which follows a '+' type tag and a typedstream length — one
// byte, or 0x81 + uint16 LE, or 0x82 + uint32 LE.
func decodeAttributedBody(blob []byte) string {
	i := bytes.Index(blob, []byte("NSString"))
	if i < 0 {
		return ""
	}
	j := bytes.IndexByte(blob[i:], '+')
	if j < 0 {
		return ""
	}
	p := i + j + 1
	if p >= len(blob) {
		return ""
	}
	n := int(blob[p])
	p++
	switch n {
	case 0x81:
		if p+2 > len(blob) {
			return ""
		}
		n = int(binary.LittleEndian.Uint16(blob[p:]))
		p += 2
	case 0x82:
		if p+4 > len(blob) {
			return ""
		}
		n = int(binary.LittleEndian.Uint32(blob[p:]))
		p += 4
	}
	if n < 0 || p+n > len(blob) {
		return ""
	}
	return string(blob[p : p+n])
}

// tapbackEmoji maps a tapback type (2000–2006) to the emoji shown.
func tapbackEmoji(t int, custom string) string {
	switch t {
	case 2000:
		return "❤️"
	case 2001:
		return "👍"
	case 2002:
		return "👎"
	case 2003:
		return "😂"
	case 2004:
		return "‼️"
	case 2005:
		return "❓"
	}
	if custom != "" {
		return custom
	}
	return "👍"
}

// mediaType maps an attachment to the UI's media types.
func mediaType(a imAttRow) string {
	switch {
	case a.Sticker == 1:
		return "sticker"
	case strings.HasPrefix(a.MimeType, "image/"):
		return "image"
	case strings.HasPrefix(a.MimeType, "video/"):
		return "video"
	case strings.HasPrefix(a.MimeType, "audio/"):
		return "audio"
	}
	return "document"
}

// DownloadMedia returns the attachment's local file: Messages has already
// downloaded it, under ~/Library/Messages/Attachments.
func (b *imessageBackend) DownloadMedia(chatID, msgID string) (string, error) {
	var atts []imAttRow
	err := query(b.db, `
SELECT maj.message_id, a.filename, a.mime_type, a.transfer_name, a.is_sticker
FROM message m JOIN message_attachment_join maj ON maj.message_id = m.ROWID
JOIN attachment a ON a.ROWID = maj.attachment_id
WHERE m.guid = `+sqlQuote(msgID)+` AND a.hide_attachment = 0
ORDER BY a.ROWID LIMIT 1`, &atts)
	if err != nil {
		return "", err
	}
	if len(atts) == 0 || atts[0].Filename == "" {
		return "", fmt.Errorf("no attachment found for this message")
	}
	path := expandUserPath(atts[0].Filename)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("attachment isn't on this Mac (still in iCloud?): %w", err)
	}
	return path, nil
}

// runAppleScript runs script with args as its `on run argv` arguments, so
// message text never has to be escaped into the script source.
func runAppleScript(script string, args ...string) error {
	cmd := exec.Command("osascript", append([]string{"-e", script}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if appLog != nil {
			appLog.Printf("osascript FAILED: %s", strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("Messages: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

const sendTextScript = `on run argv
	tell application "Messages" to send (item 1 of argv) to chat id (item 2 of argv)
end run`

const sendFileScript = `on run argv
	tell application "Messages" to send (POSIX file (item 1 of argv)) to chat id (item 2 of argv)
end run`

func (b *imessageBackend) SendText(chatID, text, replyTo string, mentions []string) error {
	return runAppleScript(sendTextScript, text, chatID)
}

// SendFile sends path, then caption (if any) as a separate message:
// AppleScript can't attach text to a file send.
func (b *imessageBackend) SendFile(chatID, path, caption, replyTo string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := runAppleScript(sendFileScript, abs, chatID); err != nil {
		return err
	}
	if caption != "" {
		return b.SendText(chatID, caption, "", nil)
	}
	return nil
}

func (b *imessageBackend) SendSticker(chatID, path, replyTo string) error {
	return b.SendFile(chatID, path, "", "")
}

var errUnsupported = fmt.Errorf("not supported by Messages")

func (*imessageBackend) MarkChatRead(string) error                               { return errUnsupported }
func (*imessageBackend) SendReact(_, _, _, _ string) error                       { return errUnsupported }
func (*imessageBackend) DeleteMessage(_, _ string, _ bool) error                 { return errUnsupported }
func (*imessageBackend) ResolveMentionNames([]string) (map[string]string, error) { return nil, nil }
func (*imessageBackend) FetchOwnMentionIDs() (map[string]bool, error)            { return nil, nil }

// contactName returns handle's (a phone number or email) name from the
// macOS Contacts databases, or handle itself if there's no match.
func (b *imessageBackend) contactName(handle string) string {
	b.contactsOnce.Do(b.loadContacts)
	if n := b.contacts[contactKey(handle)]; n != "" {
		return n
	}
	return handle
}

var nonDigits = regexp.MustCompile(`\D`)

// contactKey normalizes a phone number or email for matching: emails
// lowercased; phone numbers reduced to their last 9 digits, so "+62 812…"
// and a locally formatted "0812…" for the same number match.
func contactKey(s string) string {
	if strings.Contains(s, "@") {
		return strings.ToLower(strings.TrimSpace(s))
	}
	d := nonDigits.ReplaceAllString(s, "")
	if len(d) > 9 {
		d = d[len(d)-9:]
	}
	return d
}

// loadContacts reads every AddressBook source database once. It's best
// effort: unreadable sources are skipped and names fall back to handles.
func (b *imessageBackend) loadContacts() {
	b.contacts = map[string]string{}
	home, _ := os.UserHomeDir()
	dbs, _ := filepath.Glob(filepath.Join(home, "Library", "Application Support", "AddressBook", "Sources", "*", "AddressBook-v22.abcddb"))
	type row struct {
		Value string `json:"v"`
		First string `json:"ZFIRSTNAME"`
		Last  string `json:"ZLASTNAME"`
		Nick  string `json:"ZNICKNAME"`
		Org   string `json:"ZORGANIZATION"`
	}
	for _, db := range dbs {
		var rows []row
		err := query(db, `
SELECT p.ZFULLNUMBER AS v, r.ZFIRSTNAME, r.ZLASTNAME, r.ZNICKNAME, r.ZORGANIZATION
FROM ZABCDPHONENUMBER p JOIN ZABCDRECORD r ON r.Z_PK = p.ZOWNER
UNION ALL
SELECT e.ZADDRESS AS v, r.ZFIRSTNAME, r.ZLASTNAME, r.ZNICKNAME, r.ZORGANIZATION
FROM ZABCDEMAILADDRESS e JOIN ZABCDRECORD r ON r.Z_PK = e.ZOWNER`, &rows)
		if err != nil {
			continue
		}
		for _, r := range rows {
			name := strings.TrimSpace(r.First + " " + r.Last)
			if name == "" {
				name = r.Nick
			}
			if name == "" {
				name = r.Org
			}
			if k := contactKey(r.Value); k != "" && name != "" {
				if _, dup := b.contacts[k]; !dup {
					b.contacts[k] = name
				}
			}
		}
	}
}
