# WhatsApp MCP on Windows (x64): setup guide for Claude Code

This file is written for a Claude Code session running on the Windows machine. If you are
that session: read this file, run the checks, do each step in order, and stop at the
steps marked **USER**, which need the human (a QR scan, a download).

Goal: the `whatsapp` MCP server registered with Claude Code, and the Go bridge running
hidden in the background at logon. No C compiler, no MSYS2, no CGO.

Repo: `git@github.com:Vignesh-Thangamariappan/whatsapp-mcp.git`
(HTTPS: `https://github.com/Vignesh-Thangamariappan/whatsapp-mcp.git`)

## 0. Discover what is already there

Run these in PowerShell and note what is missing before changing anything:

```powershell
git --version
uv --version
claude --version
claude mcp list                       # is "whatsapp" already registered?
Get-ScheduledTask -TaskName WhatsAppBridge -ErrorAction SilentlyContinue
Test-Path "$HOME\whatsapp-mcp\whatsapp-bridge\whatsapp-bridge.exe"
Test-Path "$HOME\whatsapp-mcp\whatsapp-bridge\store\whatsapp.db"   # true = already paired
```

Install anything missing (needs the human's OK if it prompts):

```powershell
winget install --id Git.Git -e
winget install --id astral-sh.uv -e
# Claude Code CLI: https://docs.claude.com/en/docs/claude-code/setup
```

Open a new terminal after installing so `PATH` refreshes. You do not need Go, MSYS2 or
ffmpeg. (ffmpeg is only for voice-note sending; skip it.)

## 1. Get the code

```powershell
git clone https://github.com/Vignesh-Thangamariappan/whatsapp-mcp.git "$HOME\whatsapp-mcp"
cd "$HOME\whatsapp-mcp"
git checkout feature/windows-lightweight   # drop this once the PR is merged to main
```

If the folder already exists, `git pull` instead. Never overwrite `whatsapp-bridge\store\`:
it holds the pairing and message history.

## 2. Get the bridge exe  (USER, unless it is already there)

The exe is not committed to git. It is built on a machine with Go (the Mac) with
`bash windows/build.sh`, which produces `dist/whatsapp-bridge.exe`. Ask the user to copy
that file to:

```
%USERPROFILE%\whatsapp-mcp\whatsapp-bridge\whatsapp-bridge.exe
```

(AirDrop, a USB stick, a shared drive, whatever is easiest.) Check with `Test-Path`.

Fallback if the user cannot copy it: `winget install --id GoLang.Go -e`, then
`cd whatsapp-bridge; $env:CGO_ENABLED=0; go build -o whatsapp-bridge.exe .`. This works
without any C compiler because the SQLite driver is pure Go. It does install Go
(roughly 250 MB), which is why copying the exe is preferred.

## 3. Run the installer

```powershell
cd "$HOME\whatsapp-mcp"
powershell -ExecutionPolicy Bypass -File windows\install.ps1
```

This runs `uv sync` for the Python MCP server and registers it with
`claude mcp add -s user whatsapp ...`. It is safe to run again.

## 4. Pair the phone  (USER)

Tell the user what is about to happen, then run in a visible terminal:

```powershell
cd "$HOME\whatsapp-mcp\whatsapp-bridge"
.\whatsapp-bridge.exe
```

A QR code prints. On the phone: WhatsApp > Settings > Linked Devices > Link a Device, and
scan it within 3 minutes. When the log shows it is connected, press Ctrl+C.

This Windows machine becomes its own linked device. Never copy `whatsapp.db` from
another machine, it would clone that device's identity. Re-pairing is needed about every
20 days (WhatsApp's own limit).

If pairing fails with a "client outdated" error, the pinned whatsmeow version is too old;
see `ROADMAP.md`.

## 5. Start it at every logon

```powershell
cd "$HOME\whatsapp-mcp"
powershell -ExecutionPolicy Bypass -File windows\install.ps1 -RegisterTask
```

This registers a hidden scheduled task, caps the Go heap with `GOMEMLIMIT=64MiB`, rotates
`store\bridge.log` at 1 MB, and starts it now.

## 6. Verify

```powershell
netstat -ano | findstr :8080          # must show 127.0.0.1:8080, NOT 0.0.0.0:8080
Get-ScheduledTask -TaskName WhatsAppBridge | Select-Object State
claude mcp list                       # whatsapp should show as connected
Get-Content "$HOME\whatsapp-mcp\whatsapp-bridge\store\bridge.log" -Tail 20
```

If `:8080` shows `0.0.0.0`, stop the task (`Stop-ScheduledTask -TaskName WhatsAppBridge`)
and do not continue: the fork's loopback-only fix is not in effect.

Finally, in a new Claude Code session, ask it to list recent WhatsApp chats to confirm the
tools work end to end.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `whatsapp` shows failed in `claude mcp list` | Bridge not running. `Start-ScheduledTask -TaskName WhatsAppBridge`, check `store\bridge.log`. |
| Tools say "unauthorized" | The server reads the token from `whatsapp-bridge\store\api_token.txt`; restart the bridge so both agree. |
| Task runs but nothing listens on 8080 | Not paired yet (step 4), or the exe crashed; read `store\bridge.log`. |
| `uv` not found after install | Open a new terminal so `PATH` refreshes. |

## Remove it

```powershell
Unregister-ScheduledTask -TaskName WhatsAppBridge -Confirm:$false
claude mcp remove whatsapp -s user
```

Then unlink the device on the phone (Linked Devices) and delete the folder.
