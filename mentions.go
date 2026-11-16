package main

import "regexp"

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
