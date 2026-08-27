# Security Policy

## Reporting a vulnerability

Please **do not** open a public Issue for a security vulnerability.

Preferred: use GitHub's private reporting — go to this repo's **Security**
tab → **Report a vulnerability**. That opens a private advisory only the
maintainer can see until it's resolved.

If that's not available to you, email
**52261820+sreenathkc@users.noreply.github.com** (GitHub's private relay
— it reaches the maintainer without exposing a personal address) with a
description of the issue and, if possible, steps to reproduce. You should
get an acknowledgement within a few days — this is a maintained solo/hobby
project, not a company with an SLA, so please be patient.

## Scope and threat model

ClipHanger is designed to run on a **trusted home LAN**, not the open
internet — see `docs/DECISIONS.md` for the full reasoning. In short:

- The web UI and API are gated behind an auto-generated `X-Api-Key`
  (`docs/API.md`), not exposed with nothing in front of them.
- Media-server credentials (Plex token, Kodi/Jellyfin username-password)
  are entered once in ClipHanger's own Setup page and never leave the
  box — there is deliberately no client-facing endpoint to set or read
  them, and credentials are scrubbed before they can reach a log or an
  error string (see `CLAUDE.md`'s hard rules).
- An optional real username/password login can be enabled from Settings
  for people who want a second layer beyond the API key.

Reports about the LAN-trust model itself ("this isn't safe to expose
directly to the internet without a reverse proxy/VPN in front of it")
are expected behaviour, not a vulnerability — that's a deployment
choice documented in the README's Installation section, not a bug.
Reports about an actual flaw in that model (a credential leaking
somewhere it shouldn't, an auth check that can be bypassed, an
injection point) are very much wanted.

## Supported versions

This project doesn't yet have tagged releases — please report against
the current `main` branch, and check whether the issue is already fixed
there before filing.
