#!/bin/sh
# One-shot setup: run the wacli sync service with the patched wacli and an
# --on-message command, so every new incoming message pops a notification.
# Safe to re-run. Edit NOTIFY below to change what runs.
#
#   hooks/install.sh             set up / refresh
#   hooks/install.sh uninstall   back to Homebrew wacli, no hook
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
WACLI_SRC=$(cd "$HERE/../../wacli" && pwd)
NOTIFY='terminal-notifier -title {chat} -subtitle {sender} -message {text} -group {chat_jid} -activate net.kovidgoyal.kitty -sound default'
LA="$HOME/Library/LaunchAgents"
PLIST="$LA/sh.wacli.sync.plist"
GUI="gui/$(id -u)"

# Retire the earlier polling setups so nothing double-notifies.
for l in sh.wtui.notifyd sh.wtui.notify sh.wa-notify; do
	launchctl bootout "$GUI/$l" 2>/dev/null || true
	rm -f "$LA/$l.plist"
done

[ -f "$PLIST.orig" ] || cp "$PLIST" "$PLIST.orig"

if [ "${1:-}" = uninstall ]; then
	cp "$PLIST.orig" "$PLIST"
else
	echo "building patched wacli…"
	(cd "$WACLI_SRC" && CGO_CFLAGS=-Wno-error=missing-braces \
		go build -tags sqlite_fts5 -o dist/wacli ./cmd/wacli)

	# Rebuild ProgramArguments from scratch: patched wacli + --on-message.
	/usr/libexec/PlistBuddy \
		-c "Delete :ProgramArguments" \
		-c "Add :ProgramArguments array" \
		-c "Add :ProgramArguments: string $WACLI_SRC/dist/wacli" \
		-c "Add :ProgramArguments: string sync" \
		-c "Add :ProgramArguments: string --follow" \
		-c "Add :ProgramArguments: string --on-message" \
		-c "Add :ProgramArguments: string $NOTIFY" \
		"$PLIST"
fi
plutil -lint -s "$PLIST"

launchctl bootout "$GUI/sh.wacli.sync" 2>/dev/null || true
sleep 1
launchctl bootstrap "$GUI" "$PLIST"
sleep 2
if launchctl print "$GUI/sh.wacli.sync" >/dev/null 2>&1; then
	echo "wacli sync: running"
else
	echo "wacli sync: NOT running (see ~/Library/Logs/wacli/sync.err.log)"
fi
