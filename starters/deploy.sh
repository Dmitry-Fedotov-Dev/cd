#!/usr/bin/env bash
# Copy into a project as ./deploy.sh. Deploys with the pinned cd client:
#   CD_SERVER=https://deploy.example.com ./deploy.sh [deploy | status | logs [N] | rollback]
# Project name and server can live in .cd.env next to this script (CD_PROJECT=..., CD_SERVER=...).
set -euo pipefail
CD_VERSION=${CD_VERSION:-v1}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
if [ -f "$here/.cd.env" ]; then
  set -a
  # shellcheck source=/dev/null
  . "$here/.cd.env"
  set +a
fi
client=$(mktemp)
trap 'rm -f "$client"' EXIT
curl -fsSL "https://raw.githubusercontent.com/Dmitry-Fedotov-Dev/cd/$CD_VERSION/deploy.sh" -o "$client"
bash "$client" "$@"
