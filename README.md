# cd

## Quick start

```sh
# 1. on the server (Linux, Docker, systemd, a reverse proxy for HTTPS): install cd once
curl -fsSLO https://raw.githubusercontent.com/Dmitry-Fedotov-Dev/cd/v1/install.sh && sudo bash install.sh --public-url https://deploy.example.com

# 2. on the server: allow a repository to deploy project "app"
sudo cdctl project add app --repo you/app --type node --domain app.example.com

# 3. in the repository's folder (GitHub CLI logged in): connect it; every push to main deploys
curl -fsSLO https://raw.githubusercontent.com/Dmitry-Fedotov-Dev/cd/v1/init.sh && bash init.sh app https://deploy.example.com
```

Details: [in a project](#in-a-project), [on a server](#on-a-server), [security](SECURITY.md).

## What it is

Deploy any repository to your own server from GitHub Actions or GitLab CI — without SSH keys,
passwords or tokens in the repository. The CI job proves who it is with the OIDC token its
platform signs; the server (`cdd`) builds the release with its own Dockerfile and runs it under
fixed limits, with a health check and automatic rollback.

The deploy half of [ci](https://github.com/Dmitry-Fedotov-Dev/ci).

```
CI job ──OIDC token + tar.gz──► https://deploy.example.com (cdd behind your reverse proxy)
                                  ├─ token: signature, issuer, audience, repo + branch rules
                                  ├─ bundle: plain files only, size limits
                                  ├─ docker build with cd's template (node | static | dockerfile)
                                  ├─ swap: new container, health check, else old one back
                                  └─ streamed log … CD-RESULT: 0 deployed 1a2b3c4d5e6f
```

| Where | What you write in the project |
|---|---|
| **GitHub Actions** | `uses: Dmitry-Fedotov-Dev/cd@v1` with `server: ${{ vars.CD_SERVER }}` |
| **GitLab CI** | `extends: .cd-deploy` from [`gitlab/deploy.yml`](gitlab/deploy.yml) |
| **Any shell** | [`./deploy.sh`](starters/deploy.sh) with `CD_SERVER` and `CD_PROJECT` |

```
cmd/cdd/        the server: HTTP API, one deploy at a time
cmd/cdctl/      the server owner's tool: projects, ports, limits, proxy snippets
internal/       oidc (token checks), bundle (safe unpack), engine (build, swap, rollback)
templates/      Dockerfiles per project type — the bundle never decides how it runs
deploy.sh       the client every integration calls; action.yml wraps it for GitHub
gitlab/         GitLab include template
starters/       files to copy into a project
docs/           PROTOCOL.md — the wire protocol, for other clients and servers
```

## In a project

**GitHub, in one line** — in the repository's folder, with the [GitHub CLI](https://cli.github.com)
logged in:

```sh
curl -fsSLO https://raw.githubusercontent.com/Dmitry-Fedotov-Dev/cd/v1/init.sh && bash init.sh app https://deploy.example.com
```

[`init.sh`](init.sh) checks the server answers, adds `.github/workflows/deploy.yml` for project
`app`, sets the repository variable `CD_SERVER`, commits only that file and pushes; the first
deploy starts at once. By hand instead:

**GitHub.** Copy [`starters/github-deploy.yml`](starters/github-deploy.yml) to
`.github/workflows/deploy.yml`, set the project name, and add the repository variable
`CD_SERVER` (Settings → Secrets and variables → Actions → Variables). The workflow needs
`permissions: id-token: write`.

```yaml
permissions:
  contents: read
  id-token: write
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    steps:
      - uses: actions/checkout@v4
      - uses: Dmitry-Fedotov-Dev/cd@v1
        with:
          server: ${{ vars.CD_SERVER }}
          project: app
```

`action:` takes `deploy` (default), `status`, `logs` or `rollback`; the starter exposes them as a
manual run. `bundle:` sends a ready archive instead of `git archive`, `paths:` narrows the archive.

**GitLab.**

```yaml
include:
  - remote: https://raw.githubusercontent.com/Dmitry-Fedotov-Dev/cd/v1/gitlab/deploy.yml
deploy:
  extends: .cd-deploy
  variables: { CD_PROJECT: app }   # CD_SERVER as a CI/CD variable
  environment: production
  rules:
    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH
```

**What the server expects from the app** (type `node`): `package.json` with `scripts.start`
(and `scripts.build` if needed), listen on `$PORT` (3000), answer the health path (`/healthz`)
within 90 s, write only to `/tmp` and the data directory, fit into the memory limit.
Settings and secrets live on the server in the project's env file, never in the repository.

## On a server

Linux with Docker and systemd, and a reverse proxy (Caddy, nginx) for HTTPS.

```bash
curl -fsSLO https://raw.githubusercontent.com/Dmitry-Fedotov-Dev/cd/v1/install.sh
less install.sh                       # it touches only /usr/local/bin/cd*, /etc/cdd, /var/lib/cdd
sudo bash install.sh --public-url https://deploy.example.com
```

The installer prints the reverse proxy block. Then allow a project:

```bash
sudo cdctl project add app --repo me/app --branch main --environment production \
     --type node --domain app.example.com --data /app/data --memory 256m
sudo cdctl caddy app                  # site block for app.example.com → 127.0.0.1:<port>
sudo cdctl project list
```

Only `me/app` on `main` (in the `production` environment) can deploy `app`; nothing else can,
including other projects on the same server. To revoke: `cdctl project rm app`.

**Domains, automatically** (Porkbun DNS + Caddy). Once per server:

```bash
sudo cdctl domain setup --zone example.com --ip 203.0.113.10   # asks for the Porkbun API keys
```

From then on `--domain app.example.com` in `project add` (or `cdctl domain app` later) asks
`Create the DNS record app.example.com → 203.0.113.10? [y/N]`, writes the Caddy site to
`/etc/caddy/cd/app.caddy`, validates, reloads Caddy and waits for HTTPS. `cdctl project rm`
and `cdctl domain rm` take the name away again. cdctl creates a record only where the name
has no records at all, and deletes only records it created: the rest of the zone, and the
rest of the Caddyfile (besides one `import /etc/caddy/cd/*.caddy` line, added after a
backup), are never touched.

**Running apps:** `cdctl project list` (state of each), `cdctl top` (CPU, memory),
`cdctl stop app` / `cdctl start app` (off the air at once, deploys refused until started),
`cdctl project rm app [--purge]` (gone, with its data on `--purge`).

| Type | Build | Run |
|---|---|---|
| `node` | `npm ci`, `npm run build` if present, prune dev deps | `npm start` |
| `static` | files as they are, or `npm run build` → `dist` | Caddy file server |
| `dockerfile` | the project's own Dockerfile — enable only for projects you trust with a build | its command |

Every container: user 10001, read-only root, `/tmp` tmpfs, all capabilities dropped,
no-new-privileges, CPU/memory/PID limits, no swap, its own network, port on 127.0.0.1 only,
logs rotated. Data: `/var/lib/cdd/projects/<name>/data`. Server log: `journalctl -u cdd`.
Builds — where the project's own code first runs (`npm ci`, its build) — happen in a separate
BuildKit container with 1 GB, 1 CPU and 15 minutes (`build_memory`, `build_cpus`,
`build_timeout_min` in `/etc/cdd/cdd.json`), so a bad build cannot starve the host.

Who can do what, and how to verify a release: [SECURITY.md](SECURITY.md).

## Protocol

[docs/PROTOCOL.md](docs/PROTOCOL.md): identity, requests, the `CD-RESULT` line and exit codes,
bundle rules, the release swap. Exit codes: `0` ok · `1` failed, previous release serving ·
`2` bundle rejected · `3` busy, retry · `4` bad request · `5` not allowed.

## Develop

```bash
go test ./...
shellcheck deploy.sh install.sh init.sh starters/deploy.sh
```

Releases: push a `vX.Y.Z` tag; the workflow builds `cdd`/`cdctl` for linux amd64/arm64,
publishes checksums and moves the major tag (`v1`).
