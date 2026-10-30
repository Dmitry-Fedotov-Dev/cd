# Security

## Who controls what

| Who | Can | Cannot |
|---|---|---|
| **Server owner** (root on the server) | add and remove projects (`cdctl`), decide which repository and branch may deploy each, set its limits, domain and secrets, stop anything | — |
| **A project's repository** (its CI job on the allowed branch) | deploy, roll back, read status and logs of **that one project** | touch other projects, the host, the proxy, the limits, its own allow rule; deploy from another branch or a fork |
| **Anyone else on the internet** | `GET /v1/healthz` | everything else: without a token for this server from an allowed repository the answer is `5` |
| **This repository's maintainers** | publish new versions of `cdd`, `cdctl` and the client | reach any server: a server runs the version its owner installed and updates only when the owner reinstalls |

The one secret cd keeps is the server owner's own DNS API key (`cdctl domain setup`): in a
root-only file (`/etc/cdd/porkbun.json`, 0600), read only by `cdctl`, never by `cdd` or a
project. It is typed in on the server and never passes through CI. Restrict it in Porkbun to
the one domain.

Deploys have no shared secret anywhere. A deploy is authorised by the OIDC token the CI platform
signs for one job: it names the repository, branch and run, is valid for minutes and only for
the server it was requested for (`aud`). Nothing to leak from the repository, nothing to rotate.

## What a deployed project's code can do

The project's code runs twice, and both times boxed in:

- **Build** (`npm ci`, `npm run build`, or the project's Dockerfile when the owner enables type
  `dockerfile`): in a dedicated BuildKit container (`cd-builder`) with `build_memory` (1g) and
  `build_cpus` (1), at most `build_timeout_min` (15) minutes. It has network access (to fetch
  packages) but no host mounts and no secrets.
- **Run**: user 10001, read-only root filesystem, `/tmp` in memory, all capabilities dropped,
  no-new-privileges, the project's CPU/memory/PID limits, no swap, its own Docker network,
  its port on 127.0.0.1 only, logs rotated. Writable: `/tmp` and its own data directory.

A repository cannot create projects, names or ports: only the server owner does
(`cdctl project add`). It deploys into its one project, at most `deploys_per_hour` (6) deploys
and rollbacks an hour, one build at a time on the whole server; each deploy replaces the
previous release (two images kept). Whatever it serves under its own name — any number of
paths — runs inside that one container and its limits. Not capped: the size of its data
directory and its outbound traffic; give a project to people you trust.

Type `dockerfile` lets the project choose its base image and build steps; the run limits
still apply. Enable it only for projects you trust with a build.

## Verifying a release

Every release archive carries a provenance attestation signed by GitHub: which workflow, from
which commit of this repository, built it.

```sh
gh attestation verify cd_1.1.0_linux_amd64.tar.gz -R Dmitry-Fedotov-Dev/cd
```

`install.sh` checks the archive against `checksums.txt`. Pin a version
(`install.sh` from a `vX.Y.Z` tag, `uses: Dmitry-Fedotov-Dev/cd@vX.Y.Z`) to take updates
only when you choose; `@v1` follows compatible releases.

## Reporting a vulnerability

Privately, through **Security → Report a vulnerability** on this repository — not in a public
issue. You get an answer within a week.
