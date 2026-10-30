# cd protocol v1

A CI job talks to a cd server (`cdd`) over HTTPS. There is no shared secret: the job proves
who it is with the OIDC token its platform issues, and the server owner decides, per project,
which repositories and branches may deploy it.

## Identity

The job requests an OIDC token with **audience = the server's public URL**
(`https://deploy.example.com`, no trailing slash) and sends it as `Authorization: Bearer <token>`.

| Platform | How the job gets the token |
|---|---|
| GitHub Actions | `permissions: id-token: write`; the client calls `$ACTIONS_ID_TOKEN_REQUEST_URL&audience=<server>` |
| GitLab CI | `id_tokens: { CD_ID_TOKEN: { aud: <server> } }` |
| Anything else | any token from an issuer listed in the server's `issuers`, passed as `CD_TOKEN` |

The server checks:

1. Signature (RS256) against the issuer's published keys (`/.well-known/openid-configuration` → `jwks_uri`).
2. `iss` is in the server's allowed issuers.
3. `aud` equals the server's `public_url`: a token minted for another server is useless here.
4. `exp`/`nbf` with a one-minute leeway.
5. The project's `allow` rules: at least one rule whose every claim matches exactly, e.g.

```json
{"iss": "https://token.actions.githubusercontent.com", "repository": "me/app", "ref": "refs/heads/main"}
{"iss": "https://gitlab.com", "project_path": "me/app", "ref": "main", "ref_type": "branch", "ref_protected": "true"}
```

"No such project" and "not allowed" get the same answer, so project names do not leak.

## Requests

All actions are `POST {server}/v1/{action}?project={name}`.

| Action | Extra | Body |
|---|---|---|
| `deploy` | `&sha={commit}` (7–64 hex) | the bundle, `Content-Type: application/gzip` |
| `status` | — | — |
| `logs` | `&n={lines}` (1–1000) | — |
| `rollback` | — | — |

`GET {server}/v1/healthz` → `{"ok":true,"version":"…"}` (no auth).

## Answers

`text/plain`, streamed while the server works (build output included). The **last line** is
always:

```
CD-RESULT: <code> <message>
```

| Code | Meaning | Client should |
|---|---|---|
| 0 | done; for deploy: the new release is serving and healthy | succeed |
| 1 | build or start failed; the previous release is serving | fail |
| 2 | bundle rejected (format, size, paths, missing files) | fail, fix the bundle |
| 3 | busy (another deploy) or short of resources | retry later |
| 4 | bad request (action, sha, parameters) | fail |
| 5 | no token, invalid token, or not allowed for this project | fail |

Errors found before work starts also set an HTTP status (400/401/403/404/413); once streaming
has started the status is 200 and only `CD-RESULT` counts. A response without a `CD-RESULT`
line means the connection broke: treat as 3 and check `status`.

## Bundle

A `tar.gz` of the source; a single top-level directory is stripped (`git archive --prefix=app/`).
Only regular files and directories; no links, device files, absolute paths or `..`.
Limits: the project's `max_bundle_mb` (default 50) compressed, 300 MB / 20 000 files unpacked.

The server builds the bundle with **its own** Dockerfile for the project type:

| Type | Needs | Runs |
|---|---|---|
| `node` | `package.json` with `scripts.start` (and `scripts.build` if there is a build) | `npm start` |
| `static` | files to serve, or `package.json` with `scripts.build` producing `static_dir` (dist) | Caddy file server |
| `dockerfile` | the project's Dockerfile (the owner enables this type deliberately) | the image's command |

The build itself runs in a separate builder with the server's build limits (memory, CPU,
time); a build over its limits fails with `1` and the previous release keeps serving.

Whatever the type, the container runs with limits the bundle cannot change: no root (10001),
read-only filesystem except `/tmp` and the data directory, all capabilities dropped,
no-new-privileges, CPU/memory/PID limits, no swap, own network, port published on
127.0.0.1 only. The app listens on `$PORT` (3000) and answers the health path within the
project's `health_timeout_sec` (90). Runtime settings and secrets stay on the server
(`env_file`), never in the bundle.

## Release swap

1. Unpack, build `cd-<project>:<sha>`.
2. Stop the current container and keep it as `cd-<project>-old`.
3. Start the new one; wait for the health path.
4. Healthy → remove the old container, record the release. Not healthy → print the new
   release's last log lines, remove it, start the old one again, answer `1`.

`rollback` swaps the previous release's image back in the same way. One deploy or rollback
runs at a time per server.
