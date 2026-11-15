# wtui

A terminal UI for browsing your own WhatsApp chats, built on top of
[wacli](https://wacli.sh). Chat list on the left, messages on the right,
with images and stickers rendered inline (and full-size, in a split pane)
if you're using [kitty](https://sw.kovidgoyal.net/kitty/).

## Requirements

- [wacli](https://wacli.sh/quickstart.html), installed, logged in, and
  running `wacli sync --follow` in the background to keep its local
  session store up to date.
- Go 1.27+ to build.
- kitty terminal (optional) for inline image/sticker previews and the
  `o` full-size split-pane viewer. Falls back to opening media with your
  OS's default viewer otherwise.

## Build

```sh
go build -o wtui .
```

## Run

```sh
./wtui
```

## Keys

| Key                  | Action                          |
| --------------------- | -------------------------------- |
| `↑`/`↓`, `j`/`k`      | move selection                   |
| `gg` / `G`            | jump to first / last             |
| `ctrl+d` / `ctrl+u`   | half-page scroll (message pane)  |
| `tab`                 | switch focus between panes       |
| `enter`, `l`, `→`     | open the selected chat (marks it read) |
| `o`                   | open the selected image/sticker full-size, in a new kitty split pane |
| `h`, `←`, `esc`       | back to the chat list             |
| `q`, `ctrl+c`         | quit                              |

Selecting an image or sticker message auto-loads and shows it inline,
right below its marker line.

## How it works

wtui never talks to WhatsApp directly — every chat, message, and media
download goes through shelling out to the `wacli` CLI, which reads and
writes its own local session store. Opening a chat sends real read
receipts via `wacli chats mark-read`.

New-message notifications live in the separate `wa-notify` project.
