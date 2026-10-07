# Security model

Recon gives remote keyboard and mouse control of your PC, so it is built to be safe even
if you expose it to the internet.

## Trust boundaries

```
Browser ──(TLS/QUIC, session cookie, CSRF token)──► Gateway ──(QUIC, pinned identity, host token)──► Host
   └──────────(direct path: hash-pinned cert + gateway-signed one-time ticket)──────────────────────┘
```

- The **gateway** authenticates people, and it is the only party that can authorise a stream. If
  the gateway is compromised, every paired host is compromised, so it runs with minimal privileges
  (below).
- A **host** trusts the gateway whose tunnel certificate matches the SPKI pin in its pairing code,
  plus any browser presenting a valid gateway-signed direct ticket.

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
  Every attempt is audited.
- **Sessions:** 256-bit random tokens stored only as SHA-256 hashes. The cookie is
  `__Host-recon` (`Secure; HttpOnly; SameSite=Strict; Path=/`). Sessions expire after 72 h idle
  and 30 days absolute, are capped at 20 per user, and changing your password signs out every
  other session.

## Request integrity

- **CSRF:** every state-changing API call needs the per-session token in `X-Recon-CSRF` and a
  same-origin `Origin`. Login and setup require a same-origin `Origin` plus a custom header.
  No CORS is enabled.
- **WebSocket / WebTransport:** need a same-origin `Origin` and a **single-use ticket** (192-bit,
  60 s, bound to user and host, stored only as a hash).
- **Headers:** a strict CSP (`script-src 'self'`, no inline script or style,
  `frame-ancestors 'none'`, `connect-src` limited to self plus known direct endpoints), COOP/COEP
  (cross-origin isolation), CORP, `nosniff`, `Referrer-Policy: no-referrer`, `X-Frame-Options:
  DENY`, Permissions-Policy, and HSTS when a real certificate is configured.
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

## Host-side safety

- Input is injected only for the single active session. When it ends, or a new session takes
  over, every held key and button is released, so nothing gets stuck down.
- FFmpeg runs as a child process with an argument list (no shell). The only free-text option (the
  window-title regex) is checked against a strict allow-list and escaped for the filtergraph,
  which blocks filter injection such as `movie=`.
- The host config holding the token is created in the user's profile with owner-only ACLs.
- The firewall rule for the direct path covers only the Private and Domain profiles and only the
  agent executable.

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

- All authenticated users can reach all hosts. Per-host permissions are not implemented.
- TOTP secrets are stored in the 0600 state file, not encrypted at rest.
- The gateway's private CA key lives in its data directory. Protect backups of
  `/var/lib/kloudit-recon`.

## Reporting

Please report vulnerabilities privately to the repository owner.
