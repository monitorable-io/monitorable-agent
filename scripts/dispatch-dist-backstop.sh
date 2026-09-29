#!/usr/bin/env bash
# dispatch-dist-backstop.sh: starts one prod run of .github/workflows/dist-backstop.yml
# through GitHub's workflow-dispatch API. The trigger for the off-infra backstop: GitHub's
# own cron fired 2 of 42 slots in its first night, so a systemd timer outside our
# infrastructure (dist-backstop-dispatch.timer) calls this every 15 minutes instead. It only
# starts the run; the workflow checks and pings healthchecks.io, so a trigger that stops
# (host down, token expired or revoked, API refusing) surfaces there as a missed ping.
# Exit 0 = GitHub accepted the dispatch (204), 1 = it did not, 2 = no usable token.
# Spec: platform docs/superpowers/specs/2026-09-28-installer-dist-backstop-design.md
#
# Token: a fine-grained PAT for monitorable-io/monitorable-agent with only Actions: read and
# write. Read from DISPATCH_TOKEN_FILE, else $CREDENTIALS_DIRECTORY/github-token (systemd
# LoadCredential). Test overrides: DISPATCH_API_URL, DISPATCH_ALLOW_HTTP=1.
set -euo pipefail

API="${DISPATCH_API_URL:-https://api.github.com}"
REPO="monitorable-io/monitorable-agent"
WORKFLOW="dist-backstop.yml"
PROTO="=https"
[ "${DISPATCH_ALLOW_HTTP:-0}" = 1 ] && PROTO="=http,https"

if [ -n "${DISPATCH_TOKEN_FILE:-}" ]; then
    TOKEN_FILE="$DISPATCH_TOKEN_FILE"
elif [ -n "${CREDENTIALS_DIRECTORY:-}" ]; then
    TOKEN_FILE="$CREDENTIALS_DIRECTORY/github-token"
else
    echo "no token: set DISPATCH_TOKEN_FILE or run under systemd with LoadCredential=github-token" >&2
    exit 2
fi
[ -r "$TOKEN_FILE" ] || { echo "token file not readable: $TOKEN_FILE" >&2; exit 2; }
# One line, GitHub's token alphabet only: it goes into a curl config line, so a quote or a
# space must never reach it. The token itself is never printed.
TOKEN="$(cat "$TOKEN_FILE")"
case "$TOKEN" in
    ''|*[!A-Za-z0-9_]*) echo "token file does not look like a GitHub token: $TOKEN_FILE" >&2; exit 2 ;;
esac

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
# The Authorization header reaches curl on stdin (-K -), never in argv, which every local
# user can read in /proc. A 5xx or a timeout is retried; a second dispatch of the same run
# is harmless (the workflow's concurrency group queues it).
status="$(printf 'header = "Authorization: Bearer %s"\n' "$TOKEN" |
    curl --proto "$PROTO" -sS --max-time 30 --retry 2 --retry-delay 2 -K - \
        -A "monitorable-dist-backstop-dispatch/1" \
        -H "Accept: application/vnd.github+json" \
        -H "X-GitHub-Api-Version: 2022-11-28" \
        -X POST --data '{"ref":"main","inputs":{"env":"prod","selftest":"none"}}' \
        -o "$WORK/body" -w '%{http_code}' \
        "$API/repos/$REPO/actions/workflows/$WORKFLOW/dispatches")" || {
    echo "dispatch request failed (curl exit $?)" >&2
    exit 1
}
if [ "$status" != 204 ]; then
    echo "dispatch refused: HTTP $status: $(head -c 300 "$WORK/body" | tr -d '\n')" >&2
    exit 1
fi
echo "dispatched dist-backstop (env=prod) on main"
