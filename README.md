# WhatsApp MCP Server

This is a Model Context Protocol (MCP) server for WhatsApp.

With this you can search and read your personal Whatsapp messages (including images, videos, documents, and audio messages), search your contacts and send messages to either individuals or groups. You can also send media files including images, videos, documents, and audio messages.

It connects to your **personal WhatsApp account** directly via the Whatsapp web multidevice API (using the [whatsmeow](https://github.com/tulir/whatsmeow) library). All your messages are stored locally in a SQLite database and only sent to an LLM (such as Claude) when the agent accesses them through tools (which you control).

Here's an example of what you can do when it's connected to Claude.

![WhatsApp MCP](./example-use.png)

> To get updates on this and other projects I work on [enter your email here](https://docs.google.com/forms/d/1rTF9wMBTN0vPfzWuQa2BjfGKdKIpTbyeKxhPMcEzgyI/preview)

> *Caution:* as with many MCP servers, the WhatsApp MCP is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). This means that project injection could lead to private data exfiltration.

## Security fixes in this fork

This fork closes several issues found in a security audit of upstream `lharries/whatsapp-mcp` (matching upstream issues [#241](https://github.com/lharries/whatsapp-mcp/issues/241), [#215](https://github.com/lharries/whatsapp-mcp/issues/215), and the [Invariant Labs writeup](https://invariantlabs.ai/blog/whatsapp-mcp-exploited) referenced in [#42](https://github.com/lharries/whatsapp-mcp/issues/42)):

- **Path traversal in `download_media`**: a document's filename (attacker-controlled, set by whoever sends you the file) could contain `../` sequences and write outside the intended `store/<chat>/` directory. Filenames are now sanitized with `filepath.Base`, `..` is rejected outright, and a containment check backstops both.
- **Bridge REST API was unauthenticated and bound to all interfaces** (`0.0.0.0:8080`): anyone on the same LAN could send messages or pull media as you. The bridge now binds to `127.0.0.1` by default and requires a bearer token on every request. The token is auto-generated on first run, persisted to `whatsapp-bridge/store/api_token.txt` (mode `0600`), and read automatically by the Python MCP server. No setup needed unless you run the two processes on different machines, in which case set `WHATSAPP_BRIDGE_TOKEN` (and `WHATSAPP_BRIDGE_HOST` if you intentionally want to expose the port) as matching env vars on both sides. The bridge no longer prints the token to stdout, since that output is what people redirect to `whatsapp.log`.
- **`send_file`/`send_audio_message` could read the bridge's own session database.** An LLM tricked by a prompt-injected WhatsApp message could otherwise be steered into calling `send_file` on `whatsapp-bridge/store/whatsapp.db` (the session keys) or `api_token.txt`, exfiltrating them via WhatsApp itself, which is full account takeover. The bridge now blocks reads of its own top-level store files while still allowing you to re-send media you've already downloaded from a chat (that lives one directory deeper, under `store/<chat>/`).
- **SQL `LIKE` search terms weren't escaped**: a `%` or `_` in a search query could match more (or different) rows than intended in contact/chat search. Wildcards are now escaped.
- **Stricter file permissions**: `store/` and downloaded media are now created `0700`/`0600` instead of `0755`/`0644`, so other local accounts on a shared machine can't read your session keys or message history.
- **Reduced logging of message content**: plaintext message bodies and the media upload response (which includes the decryption key) are no longer printed/logged; only metadata (length, media type, timestamps) is.
- **Temp file cleanup**: converted `.ogg` voice-message temp files are now deleted after sending instead of accumulating in `/tmp`.
- **Bumped `h11` and `urllib3`** to versions past known CVEs (see [ROADMAP.md](./ROADMAP.md) for the larger, breaking `mcp`/`starlette` bump this doesn't cover yet).

Not fixed here (design-level, not a bug): incoming message/group-chat text still reaches the LLM unmarked as untrusted, and `send_file`/`send_audio_message` can still read any other absolute path on the machine, since that's the actual feature (sending arbitrary local files). Both are inherent to how this MCP works today; see the lethal-trifecta caution above. Treat any WhatsApp content (messages, group names, contact names) as untrusted input when writing prompts or automations against this server.

See [ROADMAP.md](./ROADMAP.md) for what's next, including a connection-breaking dependency issue worth knowing about before you pair a real account.

## Installation

### Prerequisites

- Go
- Python 3.6+
- Anthropic Claude Desktop app (or Cursor)
- UV (Python package manager), install with `curl -LsSf https://astral.sh/uv/install.sh | sh`
- FFmpeg (_optional_) - Only needed for audio messages. If you want to send audio files as playable WhatsApp voice messages, they must be in `.ogg` Opus format. With FFmpeg installed, the MCP server will automatically convert non-Opus audio files. Without FFmpeg, you can still send raw audio files using the `send_file` tool.

### Steps

1. **Clone this repository**

   ```bash
   git clone https://github.com/lharries/whatsapp-mcp.git
   cd whatsapp-mcp
   ```

2. **Run the WhatsApp bridge**

   Navigate to the whatsapp-bridge directory and run the Go application:

   ```bash
   cd whatsapp-bridge
   go run main.go
   ```

   The first time you run it, you will be prompted to scan a QR code. Scan the QR code with your WhatsApp mobile app to authenticate.

   After approximately 20 days, you will might need to re-authenticate.

3. **Connect to the MCP server**

   Copy the below json with the appropriate {{PATH}} values:

   ```json
   {
     "mcpServers": {
       "whatsapp": {
         "command": "{{PATH_TO_UV}}", // Run `which uv` and place the output here
         "args": [
           "--directory",
           "{{PATH_TO_SRC}}/whatsapp-mcp/whatsapp-mcp-server", // cd into the repo, run `pwd` and enter the output here + "/whatsapp-mcp-server"
           "run",
           "main.py"
         ]
       }
     }
   }
   ```

   For **Claude**, save this as `claude_desktop_config.json` in your Claude Desktop configuration directory at:

   ```
   ~/Library/Application Support/Claude/claude_desktop_config.json
   ```

   For **Cursor**, save this as `mcp.json` in your Cursor configuration directory at:

   ```
   ~/.cursor/mcp.json
   ```

4. **Restart Claude Desktop / Cursor**

   Open Claude Desktop and you should now see WhatsApp as an available integration.

   Or restart Cursor.

### Windows (x64)

Short on time, or pointing Claude Code at it? Give it [`WINDOWS_SETUP.md`](WINDOWS_SETUP.md), a step-by-step guide written for an agent.

The bridge uses the pure-Go `modernc.org/sqlite` driver, so it needs **no CGO, no C compiler and no Go toolchain on the Windows machine**. Build the exe elsewhere and copy it over:

1. On any machine with Go: `bash windows/build.sh` produces `dist/whatsapp-bridge.exe`.
2. Copy the repo to the Windows machine and put the exe at `whatsapp-bridge\whatsapp-bridge.exe`.
3. Install [`uv`](https://docs.astral.sh/uv/) and the Claude Code CLI, then run `powershell -ExecutionPolicy Bypass -File windows\install.ps1`. This sets up the Python server and registers the MCP.
4. Pair once from a visible terminal: `cd whatsapp-bridge; .\whatsapp-bridge.exe`, scan the QR code, then Ctrl+C.
5. Run `windows\install.ps1 -RegisterTask` to start the bridge hidden at every logon (idles around 12 MB, heap capped with `GOMEMLIMIT`).

ffmpeg is only needed for `send_audio_message`; skip it otherwise. Each machine is its own linked device and needs its own QR pairing. Do not copy `store\whatsapp.db` between machines.

## Architecture Overview

This application consists of two main components:

1. **Go WhatsApp Bridge** (`whatsapp-bridge/`): A Go application that connects to WhatsApp's web API, handles authentication via QR code, and stores message history in SQLite. It serves as the bridge between WhatsApp and the MCP server.

2. **Python MCP Server** (`whatsapp-mcp-server/`): A Python server implementing the Model Context Protocol (MCP), which provides standardized tools for Claude to interact with WhatsApp data and send/receive messages.

### Data Storage

- All message history is stored in a SQLite database within the `whatsapp-bridge/store/` directory
- The database maintains tables for chats and messages
- Messages are indexed for efficient searching and retrieval

## Usage

Once connected, you can interact with your WhatsApp contacts through Claude, leveraging Claude's AI capabilities in your WhatsApp conversations.

### MCP Tools

Claude can access the following tools to interact with WhatsApp:

- **search_contacts**: Search for contacts by name or phone number
- **list_messages**: Retrieve messages with optional filters and context
- **list_chats**: List available chats with metadata
- **get_chat**: Get information about a specific chat
- **get_direct_chat_by_contact**: Find a direct chat with a specific contact
- **get_contact_chats**: List all chats involving a specific contact
- **get_last_interaction**: Get the most recent message with a contact
- **get_message_context**: Retrieve context around a specific message
- **send_message**: Send a WhatsApp message to a specified phone number or group JID
- **send_file**: Send a file (image, video, raw audio, document) to a specified recipient
- **send_audio_message**: Send an audio file as a WhatsApp voice message (requires the file to be an .ogg opus file or ffmpeg must be installed)
- **download_media**: Download media from a WhatsApp message and get the local file path

### Media Handling Features

The MCP server supports both sending and receiving various media types:

#### Media Sending

You can send various media types to your WhatsApp contacts:

- **Images, Videos, Documents**: Use the `send_file` tool to share any supported media type.
- **Voice Messages**: Use the `send_audio_message` tool to send audio files as playable WhatsApp voice messages.
  - For optimal compatibility, audio files should be in `.ogg` Opus format.
  - With FFmpeg installed, the system will automatically convert other audio formats (MP3, WAV, etc.) to the required format.
  - Without FFmpeg, you can still send raw audio files using the `send_file` tool, but they won't appear as playable voice messages.

#### Media Downloading

By default, just the metadata of the media is stored in the local database. The message will indicate that media was sent. To access this media you need to use the download_media tool which takes the `message_id` and `chat_jid` (which are shown when printing messages containing the meda), this downloads the media and then returns the file path which can be then opened or passed to another tool.

## Technical Details

1. Claude sends requests to the Python MCP server
2. The MCP server queries the Go bridge for WhatsApp data or directly to the SQLite database
3. The Go accesses the WhatsApp API and keeps the SQLite database up to date
4. Data flows back through the chain to Claude
5. When sending messages, the request flows from Claude through the MCP server to the Go bridge and to WhatsApp

## Troubleshooting

- If you encounter permission issues when running uv, you may need to add it to your PATH or use the full path to the executable.
- Make sure both the Go application and the Python server are running for the integration to work properly.

### Authentication Issues

- **QR Code Not Displaying**: If the QR code doesn't appear, try restarting the authentication script. If issues persist, check if your terminal supports displaying QR codes.
- **WhatsApp Already Logged In**: If your session is already active, the Go bridge will automatically reconnect without showing a QR code.
- **Device Limit Reached**: WhatsApp limits the number of linked devices. If you reach this limit, you'll need to remove an existing device from WhatsApp on your phone (Settings > Linked Devices).
- **No Messages Loading**: After initial authentication, it can take several minutes for your message history to load, especially if you have many chats.
- **WhatsApp Out of Sync**: If your WhatsApp messages get out of sync with the bridge, delete both database files (`whatsapp-bridge/store/messages.db` and `whatsapp-bridge/store/whatsapp.db`) and restart the bridge to re-authenticate.

For additional Claude Desktop integration troubleshooting, see the [MCP documentation](https://modelcontextprotocol.io/quickstart/server#claude-for-desktop-integration-issues). The documentation includes helpful tips for checking logs and resolving common issues.
