package main

import (
	"regexp"
	"sort"
	"strings"
)

// mentionRe matches a WhatsApp @mention token as it's actually embedded in
// message text: "@" followed by the mentioned participant's LID (or, for
// older messages, their bare phone number), both of which are a run of
// digits with no separators. wacli's message JSON has no separate
// mentioned-JID list, so this is the only way to find them.
var mentionRe = regexp.MustCompile(`@(\d{7,20})`)

// mentionIDs returns the distinct mention IDs embedded in text that aren't
// already resolved in known.
func mentionIDs(text string, known map[string]string) []string {
	var ids []string
	seen := map[string]bool{}
	for _, m := range mentionRe.FindAllStringSubmatch(text, -1) {
		id := m[1]
		if seen[id] || known[id] != "" {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// expandMentions replaces each "@<id>" mention token in text with
// "@<name>" for every id present in names, leaving unresolved mentions as
// their raw ID (wacli has no name for them yet, so the ID is all there is
// to show).
func expandMentions(text string, names map[string]string) string {
	if len(names) == 0 {
		return text
	}
	return mentionRe.ReplaceAllStringFunc(text, func(tok string) string {
		if name, ok := names[tok[1:]]; ok {
			return "@" + name
		}
		return tok
	})
}

// mentionCandidate is a chat participant offered as an autocomplete match
// while composing a message with "@".
type mentionCandidate struct {
	phone string // bare phone digits: both the embedded "@<phone>" token and wacli's --mention target
	name  string // SenderName, shown in the dropdown
}

// mentionCandidatesFor returns the open chat's distinct non-self senders
// whose name contains query (case-insensitively), name-sorted and capped to
// 8 so the dropdown always fits on one line. Senders only seen under a LID
// (no plain phone-number JID) are skipped: there's no phone digits to embed
// as a mention token for them.
func mentionCandidatesFor(msgs []Message, query string) []mentionCandidate {
	seen := map[string]bool{}
	var all []mentionCandidate
	for _, msg := range msgs {
		if msg.FromMe {
			continue
		}
		phone, domain, ok := strings.Cut(msg.SenderJID, "@")
		if !ok || domain != "s.whatsapp.net" || phone == "" || seen[phone] {
			continue
		}
		name := strings.TrimSpace(msg.SenderName)
		if name == "" {
			continue
		}
		seen[phone] = true
		all = append(all, mentionCandidate{phone: phone, name: name})
	}
	sort.Slice(all, func(i, j int) bool {
		return strings.ToLower(all[i].name) < strings.ToLower(all[j].name)
	})
	q := strings.ToLower(query)
	var out []mentionCandidate
	for _, c := range all {
		if q != "" && !strings.Contains(strings.ToLower(c.name), q) {
			continue
		}
		out = append(out, c)
		if len(out) == 8 {
			break
		}
	}
	return out
}

// activeMentionQuery returns the text typed after an in-progress "@mention"
// at the very end of value, and whether one is active. A mention starts at
// a bare "@" preceded by start-of-text or whitespace, and stops being
// active as soon as a space or newline follows it.
func activeMentionQuery(value string) (query string, ok bool) {
	at := strings.LastIndexByte(value, '@')
	if at < 0 {
		return "", false
	}
	if at > 0 {
		switch value[at-1] {
		case ' ', '\n':
		default:
			return "", false
		}
	}
	rest := value[at+1:]
	if strings.ContainsAny(rest, " \n") {
		return "", false
	}
	return rest, true
}

// pendingMentionPhones returns the distinct phone numbers embedded as
// "@<digits>" tokens in text, for passing to wacli's --mention flag so the
// recipient's client actually highlights and pings them.
func pendingMentionPhones(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range mentionRe.FindAllStringSubmatch(text, -1) {
		id := m[1]
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
