#!/usr/bin/env bash
#
# Manual deploy to a self-hosted Synology NAS (2026-08-22, per direct
# request — "just tell me how to deploy manually, using a script").
# Synology-specific, not a generic NAS/Linux-box script — see the
# passwordless-sudo and docker-path notes below, both particular to how
# Synology's own Docker/Container Manager package is set up (renamed
# 2026-08-23 from deploy-to-nas.sh to make that explicit). Deliberately
# simple otherwise: no CI, no image registry, nothing public — matches
# where this project actually is today. Run this by hand whenever you
# want to push a new version.
#
# What it does:
#   1. Tars this repo's source (minus .git and any local data/) and
#      pipes it over SSH to REMOTE_PATH/app on the NAS — NOT rsync.
#      macOS ships Apple's own `openrsync`, not GNU rsync, and it was
#      confirmed 2026-08-22 to simply fail SSH pubkey auth when it
#      spawns its own ssh subprocess (plain `ssh` to the same host
#      works fine every time; `rsync -e ssh` to the identical host
#      fails every time, even with zero extra arguments) — a real,
#      reproducible openrsync bug/incompatibility, not a config
#      mistake. tar-over-ssh only ever uses plain ssh, which is the
#      thing actually proven to work. The remote app dir is cleared
#      (except data/) before each extract, for the same "no stale
#      files left behind" property rsync --delete would have given.
#   2. Runs `docker compose up -d --build` there, using the NAS's own
#      Docker/Container Manager — nothing is built on this Mac (it
#      doesn't have Docker installed).
#   3. Prints the resulting container status so you can see it's up.
#
# Requires: passwordless SSH key access to the NAS (see
# ~/.ssh/cliphanger_nas_deploy on this Mac — its public half needs to
# be in the NAS user's ~/.ssh/authorized_keys), Docker/Container Manager
# installed there already, and passwordless sudo for the docker binary
# specifically (Synology's Docker package doesn't create a `docker`
# group the way a typical Linux install does, so the deploy user can't
# touch /var/run/docker.sock directly — confirmed 2026-08-22 against
# this NAS). One-time setup for that last part, run once on the NAS:
#   sudo sh -c 'echo "USERNAME ALL=(ALL) NOPASSWD: /usr/local/bin/docker" >> /etc/sudoers.d/cliphanger-deploy'
#
# Usage:
#   ./deploy-to-synology-nas.sh [user@host] [remote_path]
#   ./deploy-to-synology-nas.sh you@192.168.1.50 /volume1/docker/cliphanger
#
# Both arguments are optional if CLIPHANGER_NAS_REMOTE (and optionally
# CLIPHANGER_NAS_PATH) are set in your OWN shell profile — not this
# file, which is checked into version control — so you can just run
# `./deploy-to-synology-nas.sh` with no arguments day to day without
# your NAS's host/user living in the repo:
#   export CLIPHANGER_NAS_REMOTE=you@192.168.1.50
#   export CLIPHANGER_NAS_PATH=/volume1/docker/cliphanger   # optional, this is the default

set -euo pipefail

REMOTE="${1:-${CLIPHANGER_NAS_REMOTE:-}}"
REMOTE_PATH="${2:-${CLIPHANGER_NAS_PATH:-/volume1/docker/cliphanger}}"
SSH_KEY="$HOME/.ssh/cliphanger_nas_deploy"

if [ -z "$REMOTE" ]; then
  echo "Usage: ./deploy-to-synology-nas.sh [user@host] [remote_path]" >&2
  echo "   or: export CLIPHANGER_NAS_REMOTE=you@your-nas-ip (in your own shell profile, not this repo)" >&2
  exit 1
fi

# Confirmed 2026-08-22 against this specific NAS (DS923+, x86_64):
# `docker` isn't on PATH for a non-interactive `ssh host command`
# session (only in an interactive login shell), so every remote
# invocation needs the full path rather than relying on PATH. `sudo -n`
# (never prompt, fail fast) uses the NOPASSWD sudoers rule above —
# there's no group-based access to the docker socket on this NAS.
DOCKER="sudo -n /usr/local/bin/docker"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

ssh_remote() {
  ssh -i "$SSH_KEY" -o BatchMode=yes "$REMOTE" "$@"
}

echo "==> Syncing source to $REMOTE:$REMOTE_PATH/app"
ssh_remote "mkdir -p '$REMOTE_PATH/app/data'"
# The container runs as uid 1000 (see Dockerfile), not whatever this
# SSH user's uid is — without this, the container crash-loops on first
# boot with "mkdir /data/media: permission denied" (confirmed
# 2026-08-22, a real first-deploy failure). `|| true`: once the
# container's actually run, it owns the files it created (uid 1000, not
# this SSH user), so a later deploy legitimately can't chmod those
# anymore — confirmed 2026-08-22 too, on the very next deploy after the
# first. That's fine; the container already has full access to its own
# files at that point, nothing left to fix.
ssh_remote "chmod -R 777 '$REMOTE_PATH/app/data'" || true
# Clear everything except data/ before laying down the fresh tree, so a
# file removed locally doesn't linger on the NAS forever (same property
# rsync --delete would give).
ssh_remote "cd '$REMOTE_PATH/app' && find . -mindepth 1 -maxdepth 1 ! -name data -exec rm -rf {} +"
# COPYFILE_DISABLE stops macOS's tar from embedding AppleDouble/xattr
# metadata that Linux's tar on the NAS doesn't understand — harmless
# either way (just ignored with a warning), but cleaner without it.
COPYFILE_DISABLE=1 tar czf - -C "$SCRIPT_DIR" --exclude='.git' --exclude='./data' . \
  | ssh_remote "tar xzf - -C '$REMOTE_PATH/app'"

echo "==> Building and (re)starting on the NAS — this runs the NAS's own Docker, not anything local"
ssh_remote "cd '$REMOTE_PATH/app' && $DOCKER compose up -d --build"

echo "==> Container status"
ssh_remote "$DOCKER ps --filter name=cliphanger --format 'table {{.Names}}\t{{.Status}}\t{{.Ports}}'"

echo
echo "Done. Open http://<nas-ip>:8420 to finish setup (API key, media servers) if this is the first deploy."
