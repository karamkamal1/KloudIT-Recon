# Security model

Recon gives remote keyboard and mouse control of your PC, so it is built to be safe even
if you expose it to the internet.

## Trust boundaries

```
Browser ──(TLS/QUIC, session cookie, CSRF token)──► Gateway ──(QUIC, pinned identity, host token)──► Host
   └──────────(direct path: hash-pinned cert + gateway-signed one-time ticket)──────────────────────┘
   └──────────(UDP relay: same QUIC end to end, datagrams forwarded by the gateway)─────────────────┘
```

- The **gateway** authenticates people, and it is the only party that can authorise a stream. If
  the gateway is compromised, every paired host is compromised, so it runs with minimal privileges
  (below).
- A **host** trusts the gateway whose tunnel certificate matches the SPKI pin in its pairing code,
  plus any browser presenting a valid gateway-signed direct ticket (or relay ticket, bound to the
  relay allocation the browser arrives through).

## Authentication

- **Passwords:** Argon2id (46 MiB, t=1, p=1, OWASP-recommended), constant-time comparison. Unknown
  usernames burn the same time. At most 2 hashes run at once, so a burst of logins can't exhaust
  the RAM of a small LXC.
- **2FA:** TOTP (RFC 6238, tested against the RFC vectors), ±30 s skew, replay protection (a code
  can't be used twice).
- **First run:** the admin account can only be created with a random **setup token** that is
  printed in the log and stored in a 0600 file, which is deleted after use. Someone else on your
  LAN can't claim a fresh gateway.
- **Brute force:** per-IP token bucket (10/min) plus exponential lockout per user+IP after 5
  failures (1 min, doubling up to 1 h). Keying on user+IP stops attackers from locking you out.
  Behind a reverse proxy or tunnel (Cloudflare Tunnel, Nginx) that holds only while the gateway
  trusts the proxy's `X-Forwarded-For` (`-trust-proxy` / `RECON_TRUST_PROXY`): otherwise every
  request comes from the proxy's IP, all clients share one bucket and one lockout per user. An
  entry the gateway cannot read as an address or CIDR stops it at startup instead of being
  dropped.
  Every attempt is audited.
- **Sessions:** 256-bit random tokens stored only as SHA-256 hashes. The cookie is
  `__Host-recon` (`Secure; HttpOnly; SameSite=Strict; Path=/`). Sessions expire after 72 h idle
  and 30 days absolute, are capped at 20 per user, and changing your password signs out every
  other session.

## Request integrity

- **CSRF:** every state-changing API call needs the per-session token in `X-Recon-CSRF` and a
  same-origin `Origin`. Login and setup require a same-origin `Origin` plus a custom header.
  No CORS is enabled.
- **WebSocket / WebTransport / UDP relay allocation:** need a same-origin `Origin` and a
  **single-use ticket** (192-bit, 60 s, bound to user and host, stored only as a hash).
- **Headers:** a strict CSP (`script-src 'self'`, no inline script or style,
  `frame-ancestors 'none'`, `connect-src` limited to self, the hosts' known direct endpoints and
  the UDP relay ports on the name the page was loaded from; with more than 32 relay ports, any
  port on that name), COOP/COEP (cross-origin isolation), CORP, `nosniff`,
  `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`, Permissions-Policy, and HSTS when a
  real certificate is configured.
- **Input limits:** JSON bodies are capped at 64 KiB with unknown fields rejected; control
  messages at 1 MiB, input events at 64 KiB, frames at 32 MiB, datagrams at 1200 B; strict parsing
  of every binary event.
- **Output:** all user- and host-supplied strings are rendered with `textContent`. Host names are
  sanitised. Cursor images must be strict base64.

## Transport security

- **TLS everywhere.** By default the gateway creates a private CA (10 years, ECDSA P-256) and
  issues its HTTPS certificate covering your hostnames and IPs. Installing `ca.crt` once removes the
  browser warnings. WebTransport uses separate short-lived certificates that are pinned by hash, so
  it works even without installing the CA.
- **Host ↔ gateway:** QUIC with TLS 1.3. The host pins the gateway's long-lived tunnel key
  (SPKI SHA-256) from the pairing code, and the gateway verifies the host's 256-bit token, stored
  hashed. Failed host logins are rate-limited and audited. Re-pairing rotates the token and
  disconnects the old agent.
- **Direct path:** the browser pins the host certificate by SHA-256 hash, which it receives over
  the authenticated gateway API. The host accepts only gateway-HMAC'd tickets that are single-use,
  60 s, origin-bound and host-bound.
- **UDP relay:** the gateway forwards the datagrams of one QUIC connection between the browser
  and the host, so it sees only ciphertext. The browser pins the host's certificate as on the
  direct path, and the TLS session keys exist only at the two ends: the relay itself can drop or
  delay packets, but not read or alter the stream. (A compromised gateway can still hand the
  browser other certificate hashes and tickets: see Trust boundaries.)
  - **Allocations** come only from a single-use relay ticket. Each is a UDP port of its own from
    `-relay-ports`, with a random 256-bit token the gateway sends the host over the
    authenticated tunnel. The host proves itself by sending that token from its relay socket
    (`bind`); nothing else can take the host side of an allocation.
  - **The host accepts only relayed sessions the gateway authorised:** its relay socket's QUIC
    server refuses connections from any address other than an allocation the gateway announced
    over the tunnel, one connection per allocation, and the session's hello must carry a
    gateway-HMAC'd ticket (single use, 60 s, origin-bound, host-bound) bound to that very
    allocation. A direct ticket does not open a relay allocation and a relay ticket does not
    open the direct path.
  - **No open reflector:** the gateway sends nothing to an address before it has proven itself.
    The host side is bound only by the token; the browser side only by a full-size QUIC Initial
    (≥ 1200 bytes) from the IP address that requested the allocation over HTTPS. After that,
    datagrams are forwarded only between these two addresses, everything else is dropped
    silently, and the answer to a `bind` (4 bytes) is smaller than the bind (36 bytes). Until
    the handshake completes, the host's QUIC stack limits what it sends to an unvalidated
    address to 3 × what it received (RFC 9000 anti-amplification). Forwarding is rate limited
    (browser → host 32 Mbit/s, host → browser 1 Gbit/s).
  - **Lifetimes and quotas:** the host must bind within 2 s, the browser must arrive within
    20 s, and an allocation ends when the host releases it or after 30 s in which the host sent
    the browser nothing (its keep-alives come every 5 s): the browser's datagrams alone do not
    keep a port. The host releases an allocation when its connection on it ends, also one that
    never completed the handshake (an Initial it could not decrypt, a failed handshake), and
    closes a connection that asks for no WebTransport session within 10 s. A user may hold at
    most 4 allocations, in use or not; the port range caps the total. Starts and ends are
    audited (`stream_start` / `stream_end` "via udp relay").
  - The `bind`/`release` token travels in clear on the gateway ↔ host path. Someone who can read
    that path could replay it from their own address only before the real `bind` arrives (the
    first valid one wins) or release the allocation (ending the session); they could already
    drop the packets. QUIC's own encryption is unaffected.

## Host-side safety

- Input is injected only for the single active session. When it ends, or a new session takes
  over, every held key and button is released, so nothing gets stuck down.
- FFmpeg runs as a child process with an argument list (no shell). The only free-text option (the
  window-title regex) is checked against a strict allow-list and escaped for the filtergraph,
  which blocks filter injection such as `movie=`.
- The host config holding the token is created in the user's profile with owner-only ACLs.
- The firewall rule for the direct path covers only the Private and Domain profiles and only the
  agent executable. The UDP relay needs no inbound rule: the host's relay socket sends to the
  gateway's allocation ports first, and it is the PC's stateful firewall (and a NAT in front of
  it) that then admits datagrams from those ports only. The socket itself refuses every other
  QUIC sender that gets through (`CONNECTION_REFUSED`, or Version Negotiation for an unknown
  QUIC version) and never opens a connection or a session for it.
- The elevated agent adds client modes to the Virtual Display Driver's settings
  (`C:\VirtualDisplayDriver\vdd_settings.xml`). The installer gives that folder an explicit,
  non-inherited ACL owned by Administrators (Administrators and SYSTEM full control, Users read),
  because a folder created under `C:\` lets every signed-in user modify it. The agent writes
  there only when the folder and the files it writes are not links (reparse points), are owned by
  Administrators, SYSTEM or TrustedInstaller, and give no one else write, delete or permission
  rights; otherwise it refuses and names the `icacls` command that fixes the folder.
- The logon task runs the agent elevated, but `host.json` and the user's PATH belong to the user,
  and any program the user runs can change them without elevation. The elevated agent therefore
  runs FFmpeg (`ffmpeg`, else next to it or on PATH) and has the helper load FFmpeg's libraries
  (`helperFFmpegDir`) only from its install folder (Program Files, or a folder the installer
  restricts to administrators, as for `recon-host.exe` itself) or from a local folder only
  administrators can change: the file or folder, the folder it is in and, for a folder, the
  files in it pass the check above, no folder above lets anyone else rename or delete its
  entries, and no part is a link. A configured path that fails is ignored for the default
  (`host config "ffmpeg" ignored` in host.log); without an elevated token (a standard user's
  agent) nothing is checked, since nothing is gained.

## Gateway hardening (systemd)

The service runs as the unprivileged `recon` user. The unit sets `NoNewPrivileges`,
`ProtectSystem=strict`, `ProtectHome`, `PrivateTmp/Devices`, a syscall filter
(`@system-service`), `MemoryDenyWriteExecute`, restricted address families, `UMask=0077`, and a
private state directory. The Docker image is distroless, non-root, with a read-only root and all
capabilities dropped.

## Audit log

`audit.log` (JSON lines, 0600) records setup, logins and failures, 2FA changes, password changes,
host add/remove/re-pair/online/auth failures, Wake-on-LAN, and stream start/end with duration.
Admins can view it in the UI.

## Known limitations

- The UDP relay matches the browser's UDP source IP against the IP of its HTTPS request. A client
  whose network uses different public IPs for TCP and UDP (some carrier-grade NATs, or TCP over
  IPv6 and UDP over IPv4) cannot lock an allocation and falls back to the QUIC splice relay. The
  same happens behind a reverse proxy for HTTPS unless the gateway trusts its `X-Forwarded-For`
  (`-trust-proxy`). The gateway logs the refused Initial's source and the expected IP once per
  allocation (`udp relay: refused a QUIC Initial from another IP ...`).

- The installer downloads FFmpeg (and, with `-InstallLibavcodec`, its LGPL libraries) from
  BtbN's "latest" GitHub release and checks it against the SHA-256 in that release's
  `checksums.sha256`. This catches a damaged download, not a replaced release: whoever can change
  that release can change both. The elevated agent runs that `ffmpeg.exe` and the helper loads
  those libraries, so BtbN's release is trusted as much as the agent's own bundle. BtbN rebuilds
  the release regularly, so no fixed hash can be pinned (the Virtual Display Driver and nefcon
  are pinned). For a build you vetted yourself, pass `-FFmpegPath` (and set `helperFFmpegDir`
  for the libraries), in a folder only administrators can change (see above).
- All authenticated users can reach all hosts. Per-host permissions are not implemented.
- TOTP secrets are stored in the 0600 state file, not encrypted at rest.
- The gateway's private CA key lives in its data directory. Protect backups of
  `/var/lib/kloudit-recon`.

## Reporting

Please report vulnerabilities privately to the repository owner.
