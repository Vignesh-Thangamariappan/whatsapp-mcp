# Roadmap

This tracks what's left after the security fix pass in this fork (see the "Security fixes
in this fork" section of the [README](./README.md)). Grounded in a `govulncheck` run,
a `pip-audit` run, and a scan of upstream's open issues/PRs, all done 2026-09-27 against
this fork's `main`. Re-run those tools before trusting the version numbers below; they
move fast.

## P0: will likely block you before you get this working at all

- ~~**Pinned `whatsmeow` is stale enough that WhatsApp may reject the connection
  outright.**~~ **Fixed 2026-09-27.** This wasn't hypothetical: the first real pairing
  attempt against this fork hit exactly this, `Client outdated (405) connect failure`,
  before a QR code ever rendered. Bumped `go.mau.fi/whatsmeow` from the
  `v0.0.0-20250318233852` (2025-03-18) pin to its `v0.0.0-20260925162019` snapshot and
  threaded `context.Context` through the five call sites the new API requires it on
  (`client.Download`, `sqlstore.New`, `container.GetFirstDevice`, `client.GetGroupInfo`,
  `Store.Contacts.GetContact`). As a side effect this also pulled in current
  `golang.org/x/net` and swapped `gorilla/websocket` for `coder/websocket`, which closes
  the two Go module CVEs in the P1 section below. If pairing still 405s after pulling
  this commit, WhatsApp has moved the goalposts again since 2026-09-27; re-run
  `go get go.mau.fi/whatsmeow@latest` and rebuild.
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
- ~~**Go side: two real, reachable CVEs**~~ **Fixed 2026-09-27**, as a side effect of the
  `whatsmeow` bump above: `github.com/gorilla/websocket` 1.5.0 (weak PRNG for the
  WebSocket mask key) is gone from the dependency graph (whatsmeow now uses
  `coder/websocket`), and `golang.org/x/net` came along to 0.59.0. Re-ran `govulncheck`
  after the bump: only Go standard-library/toolchain CVEs remain, fixed in later Go
  patches (1.26.3 through 1.26.6). That's a "keep your Go toolchain updated" note, not a
  `go.mod` change.

## whatsmeow library audit (2026-09-27)

Separately from the bridge/server code above, audited the `whatsmeow` dependency itself
(pinned commit `b3832c2bd1d1`) for issues that would justify forking it. Verdict: no fork
needed, nothing found is both reachable and unfixable in the bridge's own code.

- **No `recover()` around whatsmeow's internal per-stanza dispatch** (`client.go:925`,
  including the message-decrypt path). A malformed/unusual incoming message can panic
  and crash the whole bridge process, not just a goroutine. Reachable by any WhatsApp
  contact who messages the account. Five closed upstream issues are this exact pattern,
  each patched individually after being hit, never fixed structurally. Filed upstream as
  [tulir/whatsmeow#1272](https://github.com/tulir/whatsmeow/issues/1272) (no
  SECURITY.md or private reporting on that repo, so filed publicly as a hardening
  request rather than privately).
- Mitigated on our side with a process supervisor (macOS `launchd` job, restart-on-crash
  only, not on clean exit) so a whatsmeow-triggered crash reconnects automatically
  instead of silently leaving the bridge down. Setup hit an unrelated macOS Gatekeeper
  snag (an unsigned local Go binary launched via `launchd` hangs at `dyld` startup even
  though it runs fine from a terminal); fix is `sudo spctl --add <path to binary>`, a
  narrow per-binary allow-rule, not a general Gatekeeper bypass.
- Two lower-severity unbounded-read spots (`download.go` media downloads,
  `binary/unpack.go` frame decompression): theoretical memory-exhaustion DoS, bounded in
  practice by WhatsApp's own server-side size limits.
- Confirmed clean: no SSRF in media download (URL is always server-derived from an
  authenticated host list; peer-supplied `direct_path` is only ever a URL path fragment,
  never a host), MAC-then-decrypt ordering is correct, no `InsecureSkipVerify`, Noise
  handshake verifies the server cert, no plaintext content or key material in default
  log levels.

## Calling (researched 2026-09-27, not pursued)

Explored adding real outbound/inbound voice calling on top of this fork. Short version:
there's no good path right now without a much bigger trust surface than this fork
currently carries.

- **`meowcaller`** (github.com/purpshell/meowcaller) is the only serious candidate for a
  personal (non-Business-API) account, since it attaches directly to a `whatsmeow.Client`.
  But its README's "no fork needed" claim only covers its experimental group-call
  surface. The actual calling engine imports `github.com/polymorfa/hypermeow`, a fork of
  `tulir/whatsmeow` kept under the same package name so it type-checks as a drop-in, plus
  a forked `libsignal-protocol-go`. Both forks are maintained by the same one person as
  meowcaller itself (real identity, not anonymous, decent track record), but adopting
  meowcaller for real means abandoning the upstream `whatsmeow` this fork just audited
  and upgraded, for a 39k-line combined diff nobody's independently reviewed. Concrete
  issues found even in the parts that were reviewed: a real CVE in a pinned dependency
  (`pion/dtls/v3` GO-2026-6165, panic on a crafted handshake, fixed in v3.1.4), and no
  panic recovery anywhere in the peer-controlled packet parsing (RTP/SRTP/MLow codec),
  the same bug class as the whatsmeow finding above, meaning a malicious call peer could
  crash the bridge.
- **whatsapp-web.js** doesn't actually have shipped call support. What looked like "call
  support" is a single external contributor's unmerged, unreviewed pull request, not in
  any release. The library itself is fine and well-maintained, but there's nothing to
  switch to here yet, and switching would also mean trading the native Go bridge for a
  full Puppeteer/Chromium instance.
- Meta's official WhatsApp Business Calling API is real and mature, but it's tied to a
  Business Platform number, not a personal linked-device account like this fork uses, so
  it doesn't apply here.

Revisit only if: whatsmeow gains native call support, or someone does a real independent
audit of the `hypermeow`/`polymorfa` fork diff. Neither looks close as of this writing.

**Update 2026-09-27:** patched the two concrete findings in a fork
(`Vignesh-Thangamariappan/meowcaller`, branch `security-hardening`, not pushed yet):
bumped `pion/dtls/v3` past its CVE, and added `recover()` to every goroutine that
parses peer/relay-controlled data, so a malformed packet from a call peer no longer
crashes the whole process. Tests and `govulncheck` confirm both. This does not change
the go/no-go: the unaudited `hypermeow`/`polymorfa` diff underneath it is exactly as
unreviewed as before, and that's still what blocks actually integrating calling.

## P2: community fixes worth reviewing before writing your own

Upstream has 20+ open PRs (see issue #220). A few line up directly with gaps this audit
found or with everyday usability:

- **#319 (JID whitelist)** restricts which chats the MCP can see/act on. This is the
  closest existing work to shrinking the blast radius of "any WhatsApp contact can
  prompt-inject you," which the audit flagged as unresolved by design.
- **#346 (stdio hygiene)** stops stray diagnostic prints from corrupting the MCP's stdio
  channel, and fixes `list_chats` returning empty with `include_last_message=False`.
  Worth a look since this fork also touched logging output. Related but distinct gap
  observed directly on a real account: `list_chats`/`list_messages` do return data, but
  chats keyed by a `@lid` JID (newer WhatsApp accounts) come back with a numeric ID as
  the `name` and a null `last_message`, i.e. reads work but contact-name resolution is
  degraded for LID-based contacts. Same root cause as the media-403 cluster below (LID
  is a newer WhatsApp identifier scheme this fork's JID/contact handling doesn't fully
  resolve yet).
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
