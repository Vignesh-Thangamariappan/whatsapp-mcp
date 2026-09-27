# Roadmap

This tracks what's left after the security fix pass in this fork (see the "Security fixes
in this fork" section of the [README](./README.md)). Grounded in a `govulncheck` run,
a `pip-audit` run, and a scan of upstream's open issues/PRs, all done 2026-09-27 against
this fork's `main`. Re-run those tools before trusting the version numbers below; they
move fast.

## P0: will likely block you before you get this working at all

- **Pinned `whatsmeow` is stale enough that WhatsApp may reject the connection outright.**
  `go.mod` pins `go.mau.fi/whatsmeow@v0.0.0-20250318233852` (2025-03-18). As of this
  writing, upstream has a wave of open PRs all fixing the same "client outdated (405)"
  connect failure by bumping this dependency: #363, #355, #352, #348, #318, #308, #302,
  #299, #296, #274, #272, among others, several from this week. WhatsApp evidently keeps
  moving the minimum accepted client version, so a pin from March 2025 is a real risk to
  pairing a real account today. Fixing this means bumping `whatsmeow` *and* threading
  `context.Context` through the call sites that now require it (upstream's API changed
  shape; see issue #104 and PR #363/#355 for what that migration looks like). Try
  connecting first; if you hit a 405, this is why.
- **Media downloads may 403.** A large cluster of PRs (#361, #354, #353, #352, #351, #347,
  #324, #320, #307, #298, #273) all fix `download_media` returning 403 by capturing
  `direct_path` from the incoming message's protobuf at receive time, instead of this
  repo's current approach in `extractDirectPathFromURL` (`main.go`), which reconstructs it
  by string-splitting the stored media URL. That heuristic is apparently no longer
  reliable against how WhatsApp serves media.

## P1: dependency hygiene, real work not a version bump

- **Python side has 37 known vulnerabilities across 8 packages** per `pip-audit` run
  against this fork's locked deps: `mcp` 1.6.0 (fix: 1.28.1), `starlette` 0.46.1 (fix:
  past 1.3.1), plus `anyio`, `requests`, `click`, `idna`, `pygments`, `python-dotenv`.
  Most trace back to the `mcp[cli]` pin. Don't just bump the version: this fork's tools
  return Python dataclasses (`whatsapp.py`'s `Message`/`Chat`/`Contact`), and `mcp`
  versions past roughly 1.10 tightened Pydantic validation enough to reject those return
  shapes, per community reports on upstream issue #215's discussion. Bumping `mcp` needs
  converting those dataclasses to Pydantic models (or plain dicts) and testing every
  tool call end to end, not just editing `pyproject.toml`.
- **Go side: two real, reachable CVEs**, per `govulncheck` against this fork's call
  paths: `github.com/gorilla/websocket` 1.5.0 (weak PRNG for the WebSocket mask key, fix:
  1.5.3) and `golang.org/x/net` 0.37.0 (two CVEs, fixes in 0.53.0 and 0.55.0). Everything
  else `govulncheck` reports is a Go standard-library/toolchain CVE fixed in a later Go
  patch (1.26.3 through 1.26.6): that's a "keep your Go toolchain updated" note, not a
  `go.mod` change.

## P2: community fixes worth reviewing before writing your own

Upstream has 20+ open PRs (see issue #220). A few line up directly with gaps this audit
found or with everyday usability:

- **#319 (JID whitelist)** restricts which chats the MCP can see/act on. This is the
  closest existing work to shrinking the blast radius of "any WhatsApp contact can
  prompt-inject you," which the audit flagged as unresolved by design.
- **#346 (stdio hygiene)** stops stray diagnostic prints from corrupting the MCP's stdio
  channel, and fixes `list_chats` returning empty with `include_last_message=False`.
  Worth a look since this fork also touched logging output.
- **#281 / #279 (FTS5 search + SQLite indexes)**: real perf wins on `list_messages`
  search once your message history grows.
- **#342 / #328 / #316 (document MIME type + filename fixes)**: documents currently
  arrive as unnamed `.bin` files in some cases.
- **#320 (same-second filename collisions)**: history sync uses `time.Now()` for
  image/video/audio filenames, so two messages processed in the same second can
  collide and `download_media` can hand back the wrong file. Flagged independently by
  djinnsix during the audit too.
- **#354 (reactions/edits/captions)**: those message types are currently silently
  dropped rather than stored, per #345 and #350.

## P3: hardening not started by anyone yet

- Enforce a JID/contact allowlist at the MCP tool layer itself, not just the bridge, so
  a compromised or misconfigured MCP client can't message arbitrary numbers even if it
  reaches the bridge with a valid token.
- Mark WhatsApp-sourced text (message bodies, group names, contact names) as untrusted
  input when it's returned to the LLM, rather than as plain tool output indistinguishable
  from trusted data. This is the actual mechanism behind the Invariant Labs "WhatsApp
  takeover" writeup linked from upstream issue #42.
- An audit log for the bridge's REST endpoints. Right now the only record of who called
  `/api/send` and when is whatever hits stdout.
