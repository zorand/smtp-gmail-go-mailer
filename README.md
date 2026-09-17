# votereminder-smtp

A small, pure-standard-library Go program that sends individual voter-registration
reminder emails through a personal Gmail account over SMTP, authenticating with a
16-character app password. It paces sends, caps how many go out per run, retries
transient failures with backoff, and keeps a resume log so a large list can be
spread across days without ever double-sending.

## Requirements

- **Go** (only to build — recipients don't need it if you hand them a prebuilt binary).
- A **Gmail account with 2-Step Verification enabled** and a 16-character
  **app password** (Google Account → Security → 2-Step Verification → App passwords,
  or go straight to <https://myaccount.google.com/apppasswords>).

## Build

Pure stdlib — no dependencies to fetch.

```sh
go build -o votereminder-smtp .
```

### Cross-compile from one machine

Go cross-compiles out of the box. `CGO` is disabled automatically for cross
builds, and this program has no C dependencies, so nothing else is needed.

```sh
GOOS=windows GOARCH=amd64 go build -o dist/votereminder-smtp.exe .
GOOS=darwin  GOARCH=arm64 go build -o dist/votereminder-smtp-macos-arm64 .
GOOS=linux   GOARCH=amd64 go build -o dist/votereminder-smtp-linux-amd64 .
```

`go tool dist list` shows every valid `GOOS/GOARCH` pair.

## Files

Every path is a flag, so files can live **anywhere** — pass an absolute or
relative path. The defaults below are only relative to the current directory;
override them to keep data and secrets wherever you like.

| Flag → default | Purpose |
|----------------|---------|
| `-emailbody` → `emailbody.txt` | Message body. Supports `$name` / `$sender` (and `${name}` / `${sender}`). **Required** — the program exits if it's missing. |
| `-recipients` → `recipients.txt` | One recipient per line: `email` or `email,Name`. Blank lines and `#` comments ignored. Contains personal data — keep it out of version control. |
| `-password-file` → *(none)* | File holding the 16-char app password on one line (spaces and trailing newline stripped). **No default** — pass it explicitly (or use stdin / the interactive prompt). |
| `-sent-log` → `sent.log` | CSV resume log the program writes. |

`$name` resolves to the recipient's name, or to their email address when no name
is given. `$sender` resolves to `-from-name`.

### Keep the app password outside the repo

Storing `secret.txt` in the project directory is the easiest way to leak it into
git. Put it **outside any repository** and lock it down — the program warns if
the password file is group/world-readable (Unix) or sits inside a git working
tree.

macOS / Linux:

```sh
mkdir -p ~/.config/votereminder
( umask 077; printf '%s' 'abcd efgh ijkl mnop' > ~/.config/votereminder/secret.txt )
```

Windows (PowerShell):

```powershell
$dir = "$env:APPDATA\votereminder"
New-Item -ItemType Directory -Force $dir | Out-Null
Set-Content -NoNewline "$dir\secret.txt" "abcd efgh ijkl mnop"
icacls "$dir\secret.txt" /inheritance:r /grant:r "$($env:USERNAME):R"
```

### Version control

A `.gitignore` is included that excludes secrets, the recipient list (personal
data), the resume log, and built binaries. Keep it — and never commit a password
file even from outside the repo.

## Run

`-per-minute 20` and `-max 90` are the defaults, shown here for clarity.

### macOS / Linux

```sh
./votereminder-smtp -from you@gmail.com -from-name "Foo Bar" \
  -per-minute 20 -max 90 \
  -recipients recipients.txt -emailbody emailbody.txt \
  -password-file ~/.config/votereminder/secret.txt
```

### Windows — cmd.exe (`^` continues lines)

```bat
votereminder-smtp.exe -from you@gmail.com -from-name "Foo Bar" ^
  -per-minute 20 -max 90 -recipients recipients.txt -emailbody emailbody.txt ^
  -password-file "%APPDATA%\votereminder\secret.txt"
```

### Windows — PowerShell (`` ` `` continues lines; `.\` runs from the current dir)

```powershell
.\votereminder-smtp.exe -from you@gmail.com -from-name "Foo Bar" `
  -per-minute 20 -max 90 -recipients recipients.txt -emailbody emailbody.txt `
  -password-file "$env:APPDATA\votereminder\secret.txt"
```

Do a **dry run first** to eyeball the rendered messages — it needs no password
and doesn't write the resume log:

```sh
./votereminder-smtp -from you@gmail.com -from-name "Foo Bar" \
  -recipients recipients.txt -emailbody emailbody.txt -dry-run
```

## Supplying the app password

Three ways, none of which land in shell history or the process's environment:

- `-password-file secret.txt` (recommended, cross-platform).
- Piped on stdin: `pass show gmail-app | ./votereminder-smtp ...`
- Interactive prompt (echo-off on macOS/Linux; echoes on Windows).

## Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `-from` | *(required)* | Your Gmail address: SMTP username and envelope/From address. |
| `-from-name` | | Display name for the From header and `$sender`. |
| `-recipients` | `recipients.txt` | Recipient list file. |
| `-emailbody` | `emailbody.txt` | Message body file (`$name` / `$sender`). |
| `-subject` | *(reminder text)* | Subject line; also supports `$name` / `$sender`. |
| `-password-file` | | Read the app password from this file instead of prompting/stdin. |
| `-per-minute` | `20` | Max sends per minute. |
| `-max` | `90` | Max sends this run (conservative for free-Gmail SMTP). |
| `-sent-log` | `sent.log` | CSV resume log; used to skip duplicates. |
| `-retries` | `5` | Retry attempts per message for transient errors. |
| `-dry-run` | `false` | Render and log messages without connecting, sending, or writing the log. |

## Resume log

Every accepted send appends a CSV row to `sent.log` (a header is written when the
file is first created). Timestamps are local time, `YYYYMMDD-HHMMSS`:

```
from,to,timestamp
you@gmail.com,alex@example.com,20260917-012908
you@gmail.com,jordan@example.com,20260917-013144
```

On startup the program reads this file and **skips any address already in the
`to` column**, so stopping and re-running resumes cleanly. Legacy plain-email
lines from older versions are still recognized. To resend someone, remove their
row; to start over, delete the file (or point `-sent-log` at a fresh path).

## Sending limits & staying off the spam radar

- Free Gmail caps sending at roughly **500 recipients / rolling 24h**; the SMTP
  ceiling is contested and may be as low as ~100, which is why `-max` defaults to
  a conservative 90. Bump it only if you've confirmed your account goes higher.
- Exceeding the cap **pauses** sending (a `550 5.4.5` error) for up to ~24h; the
  program detects this and stops so it doesn't extend the block. Wait, then
  re-run — it resumes from the log.
- A `535` error means the app password is wrong or 2-Step Verification isn't on;
  the program stops immediately.
- Keep the list to people who actually opted in, keep the message personalized
  (`$name`), keep the `reply STOP` opt-out line, and don't crank `-per-minute` —
  that's what keeps a warm list from being classified as spam.

## Notes

- Cross-compiled binaries are self-contained; ship the `.exe` (or platform binary)
  alongside `emailbody.txt` and `recipients.txt`.
- `-from` must be the authenticated account or a configured send-as alias, or
  Gmail rejects the envelope sender.
