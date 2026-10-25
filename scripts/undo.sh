#!/usr/bin/env bash
# Undo what an agent did to the home folder, and see what it did.
#
#   scripts/undo.sh                     snapshots, newest first
#   scripts/undo.sh log [-n 100]        the audit log (every agent call)
#   scripts/undo.sh snapshot [reason]   take an undo point now
#   scripts/undo.sh show <id>           what changed just before snapshot <id>
#   scripts/undo.sh diff <id> [path]    what restoring <id> would change
#   scripts/undo.sh restore <id> [path] restore all of /root, or one path in it
#
# A path is relative to /root (Documents/client) or absolute (/root/...).
# Restoring first takes a snapshot of the current state, so it can be undone
# too. The browser profile (logins) is not in the snapshots.
set -euo pipefail
cd "$(dirname "$0")/.."

docker compose ps --status running keeper -q | grep -q . ||
    { echo "The keeper service isn't running (docker compose up -d)" >&2; exit 1; }
k() { docker compose exec -T keeper keeper "$@"; }

case "${1:-list}" in
    list) k list ;;
    log | show | diff | snapshot) k "$@" ;;
    restore)
        [ $# -ge 2 ] || { echo "usage: $0 restore <id> [path]" >&2; exit 2; }
        k diff "${@:2}"
        if [ -t 0 ] && [ "${YES:-}" != 1 ]; then
            read -rp "Restore? [y/N] " ok
            [ "$ok" = y ] || [ "$ok" = Y ] || { echo "Nothing changed."; exit 1; }
        fi
        k restore "${@:2}"
        ;;
    -h | --help | help) sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//' ;;
    *) echo "unknown command: $1 (try --help)" >&2; exit 2 ;;
esac
