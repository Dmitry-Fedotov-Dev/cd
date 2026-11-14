#!/usr/bin/env bash
# cd: connect this repository to a cd server. Run once, in the repository's folder:
#
#   curl -fsSLO https://raw.githubusercontent.com/Dmitry-Fedotov-Dev/cd/v1/init.sh && bash init.sh PROJECT SERVER
#   e.g.  bash init.sh app https://deploy.example.com
#
# It adds .github/workflows/deploy.yml for PROJECT (or keeps one that already deploys PROJECT
# with cd), sets the repository variable CD_SERVER
# (with the GitHub CLI, logged in), commits only that file and pushes. From then on every push
# to the repository's default branch (main, master, ...) deploys. The server's owner allows the
# repository first: cdctl project add PROJECT --repo OWNER/REPO --branch DEFAULT-BRANCH.
set -euo pipefail

ref=v1 # the cd version the workflow uses
starter="https://raw.githubusercontent.com/Dmitry-Fedotov-Dev/cd/$ref/starters/github-deploy.yml"
workflow=.github/workflows/deploy.yml

say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
fail() {
  printf 'cd init: %s\n' "$*" >&2
  exit 1
}

project=${1:-}
server=${2:-}
if [ -z "$project" ] || [ -z "$server" ]; then
  fail "usage: bash init.sh PROJECT SERVER   (e.g. bash init.sh app https://deploy.example.com)"
fi
[[ $project =~ ^[a-z][a-z0-9-]{1,40}$ ]] || fail "project name '$project': lowercase letters, digits and -"
server=${server%/}
[[ $server =~ ^https://[a-zA-Z0-9.-]+(:[0-9]+)?$ ]] || fail "server '$server': like https://deploy.example.com"

step "checking"
command -v git >/dev/null || fail "git is not installed"
command -v curl >/dev/null || fail "curl is not installed"
command -v gh >/dev/null || fail "the GitHub CLI is not installed: https://cli.github.com"
gh auth status >/dev/null 2>&1 || fail "the GitHub CLI is not logged in: run  gh auth login"
top=$(git rev-parse --show-toplevel 2>/dev/null) || fail "run this inside the repository's folder"
here=$PWD
cd "$top"
repo=$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null) ||
  fail "this folder is not a GitHub repository (or gh cannot see it)"
branch=$(git rev-parse --abbrev-ref HEAD)
# Deploys follow the default branch, whatever it is called: a starter hard-wired to main never
# runs in a repository on master, and nothing says so.
default=$(gh repo view --json defaultBranchRef -q .defaultBranchRef.name 2>/dev/null) || default=""
[ -n "$default" ] || fail "cannot read the default branch of $repo"
# A workflow that already deploys this project with cd (the starter plus the project's own tests,
# say) is kept as it is: only the server address is missing. Anything else is not ours to touch.
existing=""
started=""
if [ -e "$workflow" ]; then
  if grep -q "uses: Dmitry-Fedotov-Dev/cd@" "$workflow" && grep -Eq "^[[:space:]]*project: ${project}[[:space:]]*$" "$workflow"; then
    existing=1
  else
    fail "$workflow already exists and does not deploy project $project with cd: delete it to start over"
  fi
fi
health=$(curl -fsS --max-time 15 "$server/v1/healthz" 2>/dev/null) || fail "no cd server answers at $server"
say "repository $repo, branch $branch (deploys from $default)"
say "server     $server ($health)"
say "project    $project"

if [ -n "$existing" ]; then
  step "keeping $workflow"
  say "it already deploys project $project with cd: not changed"
else
  step "adding $workflow"
  mkdir -p "$(dirname "$workflow")"
  curl -fsSL "$starter" | sed -e "s/CHANGE-ME/$project/" -e "s/branches: \[main\]/branches: [$default]/" >"$workflow"
  grep -q "project: $project" "$workflow" || fail "could not write $workflow"
  grep -q "branches: \[$default\]" "$workflow" || fail "could not set branch $default in $workflow"
  say "deploys project $project on every push to $default"
fi

step "setting the repository variable CD_SERVER"
gh variable set CD_SERVER --repo "$repo" --body "$server"

if [ -n "$existing" ]; then
  # Nothing to commit, so no push starts the first deploy: start it by hand, the way the
  # starter's manual run does. A workflow without workflow_dispatch waits for the next push.
  step "first deploy"
  if gh workflow run "$(basename "$workflow")" --repo "$repo" --ref "$default" >/dev/null 2>&1; then
    say "started on $default"
    started=1
  else
    say "could not start it by hand: it starts with the next push to $default"
  fi
else
  step "commit and push"
  git add "$workflow"
  git commit -q -m "Deploy to $server with cd" -- "$workflow"
  git push -q
  say "pushed to $branch"
fi

# the downloaded init.sh is not part of the repository
if [ -f "$here/init.sh" ] && ! git ls-files --error-unmatch "$here/init.sh" >/dev/null 2>&1; then
  rm -f "$here/init.sh"
fi

step "done"
if [ -n "$existing" ]; then
  [ -z "$started" ] || say "The first deploy is running now: GitHub → $repo → Actions (or: gh run watch)."
elif [ "$branch" = "$default" ]; then
  say "The first deploy is running now: GitHub → $repo → Actions (or: gh run watch)."
else
  say "Deploys run from $default: merge $branch into $default to deploy."
fi
say "If it ends with 'not allowed', the server's owner has not allowed $repo for $project yet:"
say "  cdctl project add $project --repo $repo --branch $default"
