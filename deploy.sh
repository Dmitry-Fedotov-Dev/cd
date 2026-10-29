#!/usr/bin/env bash
# cd client: deploy this repository to a cd server (cdd), or ask it about the project.
#
#   CD_SERVER=https://deploy.example.com CD_PROJECT=app ./deploy.sh [deploy | status | logs [N] | rollback]
#
#   CD_SERVER   cdd's public URL (also the token audience)                       required
#   CD_PROJECT  project name on that server                                       required
#   CD_BUNDLE   a ready tar.gz to send; default: git archive of HEAD
#   CD_PATHS    only these paths go into the git archive (space-separated)
#   CD_SHA      commit id; default GITHUB_SHA / CI_COMMIT_SHA / git rev-parse HEAD
#   CD_TOKEN    an OIDC token, when not running on GitHub Actions or GitLab CI
#
# Identity: GitHub Actions — `permissions: id-token: write`; GitLab — `id_tokens: CD_ID_TOKEN`
# with `aud: $CD_SERVER`. No passwords or keys: the server trusts the CI platform's signature.
#
# Exit codes: 0 ok; 1 failed (previous release still serving); 2 bundle rejected;
# 3 busy, short of resources or unreachable — retry; 4 bad request; 5 not allowed.
set -euo pipefail

action=${1:-deploy}
lines=${2:-100}

say() { printf '%s\n' "$*" >&2; }
fail() {
  local code=$1; shift
  if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::error title=cd::$*"; else say "cd: $*"; fi
  exit "$code"
}

: "${CD_SERVER:?set CD_SERVER to the cd server URL, e.g. https://deploy.example.com}"
: "${CD_PROJECT:?set CD_PROJECT to the project name on that server}"
CD_SERVER=${CD_SERVER%/}
command -v curl >/dev/null || fail 4 "curl is required"

case "$action" in
  deploy|status|rollback) ;;
  logs) [[ $lines =~ ^[0-9]+$ ]] || fail 4 "logs N: N must be a number" ;;
  *) fail 4 "unknown action '$action' (deploy | status | logs [N] | rollback)" ;;
esac

token() {
  if [ -n "${CD_TOKEN:-}" ]; then
    printf '%s' "$CD_TOKEN"
  elif [ -n "${CD_ID_TOKEN:-}" ]; then # GitLab id_tokens
    printf '%s' "$CD_ID_TOKEN"
  elif [ -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ]; then # GitHub Actions
    curl -fsS --retry 3 -G --data-urlencode "audience=$CD_SERVER" \
      -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" "$ACTIONS_ID_TOKEN_REQUEST_URL" |
      sed -n 's/.*"value"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
  fi
}
tok=$(token)
if [ -z "$tok" ]; then
  if [ -n "${GITHUB_ACTIONS:-}" ]; then
    fail 5 "no OIDC token: add 'permissions: id-token: write' to the workflow or job"
  elif [ -n "${GITLAB_CI:-}" ]; then
    fail 5 "no OIDC token: add 'id_tokens: { CD_ID_TOKEN: { aud: \$CD_SERVER } }' to the job"
  fi
  fail 5 "no OIDC token: set CD_TOKEN (outside CI) or run on GitHub Actions / GitLab CI"
fi
[ -z "${GITHUB_ACTIONS:-}" ] || echo "::add-mask::$tok"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
url="$CD_SERVER/v1/$action?project=$CD_PROJECT"
upload=()

if [ "$action" = deploy ]; then
  sha=${CD_SHA:-${GITHUB_SHA:-${CI_COMMIT_SHA:-$(git rev-parse HEAD 2>/dev/null || true)}}}
  [[ $sha =~ ^[0-9a-f]{7,64}$ ]] || fail 4 "cannot tell the commit: set CD_SHA"
  url="$url&sha=$sha"
  if [ -n "${CD_BUNDLE:-}" ]; then
    [ -f "$CD_BUNDLE" ] || fail 4 "CD_BUNDLE=$CD_BUNDLE does not exist"
    bundle=$CD_BUNDLE
  else
    command -v git >/dev/null || fail 4 "git is required to pack the repository (or set CD_BUNDLE)"
    bundle="$tmp/bundle.tar.gz"
    # shellcheck disable=SC2086 # CD_PATHS is a list
    git archive --format=tar.gz --prefix=app/ "$sha" ${CD_PATHS:-} >"$bundle" ||
      fail 4 "git archive failed (is $sha checked out with history? set CD_BUNDLE otherwise)"
  fi
  say "cd: deploying $CD_PROJECT ${sha:0:12} ($(du -h "$bundle" | cut -f1)) to $CD_SERVER"
  upload=(-H "Content-Type: application/gzip" --data-binary "@$bundle")
elif [ "$action" = logs ]; then
  url="$url&n=$lines"
fi

set +e
curl -sS -N --connect-timeout 20 --max-time 2400 -X POST \
  -H "Authorization: Bearer $tok" -H "User-Agent: cd-client" \
  "${upload[@]}" "$url" | tee "$tmp/out"
curl_rc=${PIPESTATUS[0]}
set -e

result=$(grep -a '^CD-RESULT: ' "$tmp/out" | tail -n 1 || true)
if [ -z "$result" ]; then
  fail 3 "no answer from $CD_SERVER (curl exit $curl_rc): retry later"
fi
code=$(awk '{print $2}' <<<"$result")
msg=$(cut -d' ' -f3- <<<"$result")
[[ $code =~ ^[0-9]+$ ]] || fail 3 "garbled answer: $result"

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  mark=$([ "$code" = 0 ] && echo "✅" || echo "❌")
  echo "$mark **cd $action** \`$CD_PROJECT\` → $CD_SERVER: ${msg:-done} (code $code)" >>"$GITHUB_STEP_SUMMARY"
fi
[ "$code" = 0 ] || fail "$code" "$action $CD_PROJECT: $msg"
