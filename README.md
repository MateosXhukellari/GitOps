# gitops

A small GitOps controller for Kubernetes. It watches a git repository of plain
YAML manifests and keeps a cluster in sync with it: **git is the source of
truth, and anything that drifts gets put back.**

It is a lightweight alternative to Argo CD and Flux for teams that want
"push to git, cluster follows" without running a full platform. In testing it
used about **4 MiB of memory and 16m CPU** on a repo with 3 resources (measured
with `kubectl top` on k3s; your numbers will grow with repo size).

> **Status: early.** It has been run end to end against a real k3s cluster, but
> it is young software. Start in dry-run, read the [limits](#limits), and
> expect rough edges.

## What it does

- Polls a git repo (and syncs immediately on a signed GitHub webhook).
- Diffs the desired state against the live cluster with `kubectl diff` and applies
  changes with server-side apply.
- **Self-heals:** a manual `kubectl scale` or edit is reverted on the next cycle.
- **Prunes** objects you delete from git (opt-in, heavily guarded).
- Works with **private repos** through a read-only HTTPS token.
- Reports status as JSON (`/status`) and Prometheus metrics (`/metrics`).

It does not build images. A typical flow is: CI builds the image, CI commits the
new tag into the manifest repo, and this tool deploys it.

## Safety model

Every one of these is on by default, and you opt out explicitly:

| Safeguard | Behavior |
|---|---|
| **Dry-run first** | `-dry-run` defaults to **true**: it logs the diff and changes nothing until you pass `-dry-run=false`. |
| **Namespace allowlist** | Only namespaces in `-namespaces` may be touched. Cluster-scoped kinds (ClusterRole, CRD, ...) are rejected. A `Namespace` object is allowed only if it is allowlisted. |
| **A bad commit never applies** | One policy violation or unparseable file rejects the whole revision. Nothing is half-applied, and the last good state stays. |
| **No plaintext Secrets** | `kind: Secret` is rejected unless you pass `-allow-plaintext-secrets`. |
| **Prune is off by default** | See [Pruning](#pruning). |
| **Secrets stay out of flags and logs** | Tokens come from environment variables or mounted files only, and are redacted from logs and errors. |
| **Least-privilege RBAC** | `deploy/bootstrap.yaml` limits the controller to named namespaces and kinds. This is a second, independent layer behind the allowlist. |

## Quick start (on a cluster)

You need a cluster, `kubectl` access as an admin, and a git repo of manifests.

**1. Get the deploy files.**

```bash
git clone https://github.com/MateosXhukellari/GitOps.git && cd GitOps
```

**2. Create the permission boundary (once, as a cluster admin).** Edit
`deploy/bootstrap.yaml` so it lists the namespaces and kinds the controller may
manage, then apply it:

```bash
kubectl apply -f deploy/bootstrap.yaml
```

The controller can do nothing outside what this file grants, whatever the repo
contains. Add a kind to the `Role` only when you deploy it.

**3. Give it access to a private repo (skip for public repos).** Create a
GitHub *fine-grained* token limited to the one manifest repo with
**Contents: Read-only**, then store it without leaving it in your shell history:

```bash
read -rs GH_TOKEN
kubectl -n gitops-system create secret generic gitops-git --from-literal=token="$GH_TOKEN"
unset GH_TOKEN
```

Do not use a classic `repo`-scope token: it can write to everything you own.

**4. Edit `deploy/gitops.yaml`:**

- `GITOPS_REPO`, `GITOPS_BRANCH`, `GITOPS_PATH`: where your manifests live.
- `GITOPS_NAMESPACES`: every namespace it may manage (must match `bootstrap.yaml`).
- `image:` a pinned tag such as `mateosxh/gitops:sha-<short>`. Prefer a SHA tag
  to `latest`, so you know exactly what is running.
- Leave `GITOPS_DRY_RUN: "true"` for now.

**5. Deploy and watch.**

```bash
kubectl apply -f deploy/gitops.yaml
kubectl -n gitops-system logs deploy/gitops -f
```

You should see `reconciled ... sync=OutOfSync` plus the diffs it would apply.
When they look right, set `GITOPS_DRY_RUN: "false"` and apply again.

### Requirements for the cluster

- The image bundles `kubectl` v1.35.9; keep it within one minor version of your
  cluster, or rebuild the image with a different `KUBECTL_VERSION`.
- If your Docker Hub repo is private, add an `imagePullSecret`.

## Configuration

Settings come from flags or environment variables. A flag wins over its
environment variable, which wins over the default.

| Flag | Environment variable | Default | Meaning |
|---|---|---|---|
| `-repo` | `GITOPS_REPO` | *(required)* | Git repository URL (https for token auth) |
| `-branch` | `GITOPS_BRANCH` | `main` | Branch to track |
| `-path` | `GITOPS_PATH` | `.` | Directory in the repo containing manifests |
| `-namespaces` | `GITOPS_NAMESPACES` | *(required)* | Comma-separated namespaces it may manage |
| `-interval` | `GITOPS_INTERVAL` | `1m` | Poll interval |
| `-dry-run` | `GITOPS_DRY_RUN` | `true` | Log diffs but never apply |
| `-listen` | `GITOPS_LISTEN` | `:8080` | HTTP listen address |
| `-workdir` | `GITOPS_WORKDIR` | `/tmp/gitops/checkout` | Local checkout directory |
| `-kubectl` | `GITOPS_KUBECTL` | `kubectl` | kubectl binary (name in PATH or full path) |
| `-allow-plaintext-secrets` | `GITOPS_ALLOW_PLAINTEXT_SECRETS` | `false` | Permit `kind: Secret` manifests |
| `-prune` | `GITOPS_PRUNE` | `false` | Delete objects removed from git |
| `-prune-kinds` | `GITOPS_PRUNE_KINDS` | `deployments.apps,services,configmaps` | Kinds that may be pruned |
| `-prune-max` | `GITOPS_PRUNE_MAX` | `10` | Refuse to delete more than this many objects in one cycle |
| `-owner` | `GITOPS_OWNER` | `gitops` | Label value marking objects this instance manages; use a different value per instance |

Secrets have **no flag form**, so they never appear in `ps` output or shell
history. Set the variable, or point `..._FILE` at a mounted file:

| Environment variable | Purpose |
|---|---|
| `GITOPS_GIT_TOKEN` / `GITOPS_GIT_TOKEN_FILE` | HTTPS token for a private repo (sent only over https, scoped to the repo's host) |
| `GITOPS_WEBHOOK_SECRET` / `GITOPS_WEBHOOK_SECRET_FILE` | Shared secret to verify GitHub webhooks |

Run `gitops -h` for the full list.

## Webhooks (optional)

Polling works with no network setup. For immediate sync on push, expose port
8080 (for example through an Ingress) and add a GitHub webhook:

- **Payload URL:** `https://<your-host>/webhook`
- **Content type:** `application/json`
- **Secret:** the same value as `GITOPS_WEBHOOK_SECRET`

Requests must carry a valid `X-Hub-Signature-256` header. Without a configured
secret the endpoint accepts unsigned requests, so only run that way on a
trusted network. Keep polling on as the fallback.

## Pruning

Off by default. Enable it with `-prune` once you trust the diffs.

- It only deletes objects labelled `gitops.rta.io/owner=<owner>`, a label the
  tool adds to everything it applies. Objects created by hand or by other tools
  carry no label and are never touched.
- It only looks at the allowlisted namespaces and the kinds in `-prune-kinds`.
  Namespaces are never pruned.
- It runs only after a successful apply, and never on an empty revision (a wrong
  path or an emptied repo cannot wipe the cluster).
- It lists first and deletes after, so a failed listing deletes nothing.
- It refuses to delete more than `-prune-max` objects in one cycle.
- In dry-run it only logs `would prune`.
- The controller's Role needs the `delete` verb for each pruned kind. Without
  it you get a clean `Forbidden` error and nothing is deleted.

Existing objects are adopted (labelled) the first time a cycle runs with prune on.

## Observability

Every successful cycle logs one line:

```
level=INFO msg=reconciled revision=fa13abb resources=3 sync=Synced applied=false pruned=0 took=27ms
```

Failures are labelled by kind: `git`, `manifest`, `policy`, `apply` or `prune`.

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | Liveness and readiness |
| `GET /status` | JSON: revision, sync state (`Synced`, `OutOfSync`, `Unknown`), last error and its kind, counters |
| `GET /metrics` | Prometheus text format |
| `POST /webhook` | Trigger an immediate sync (signed) |

Metrics: `gitops_reconcile_total`, `gitops_applies_total`, `gitops_pruned_total`,
`gitops_resources`, `gitops_out_of_sync`, `gitops_last_success_timestamp_seconds`,
and `gitops_reconcile_failures_total{kind="..."}`.

### Alerting

Alert on either of these:

```promql
increase(gitops_reconcile_failures_total[10m]) > 0     # any failed cycle
time() - gitops_last_success_timestamp_seconds > 300   # no good cycle for 5 minutes
```

Do not alert on `gitops_out_of_sync` alone: it is only `1` in dry-run (a diff
was found and left alone), and it stays `0` while cycles are failing.

## Limits

Please read these before pointing it at anything important.

- **Plain YAML only.** No Helm or Kustomize.
- **Secrets are not supported in git.** Plaintext Secrets are rejected on
  purpose. Create them out of band, or use an encrypted-secret tool in the
  cluster (Sealed Secrets, SOPS) once it is supported here.
- **Identity fields only.** Each document is parsed with a real YAML parser
  (`gopkg.in/yaml.v3`), but only `apiVersion`, `kind`, `metadata.name` and
  `metadata.namespace` are read; the document is sent to kubectl exactly as
  written. A document that cannot be identified (invalid YAML, not a mapping,
  missing fields, content on a `---` line) is an error naming the file and
  document number, never a silent skip.
- **Single cluster, single repo path per instance.** Run several instances,
  each with its own `-owner`, for several sources.
- **No health assessment** (it does not wait for a Deployment to become
  ready) and no UI.
- **amd64 image only** for now.

## Development

```bash
go test -race ./...      # unit tests
go vet ./... && gofmt -l .
```

Requirements: Go 1.24+, `git`, and `kubectl` on the PATH for a local run. The
only dependency outside the standard library is `gopkg.in/yaml.v3`.

Run locally against your current kube context (dry-run by default):

```bash
go build -o gitops ./cmd/gitops
./gitops -repo https://github.com/<you>/<manifests>.git -path manifests -namespaces demo
```

Layout:

```
cmd/gitops/          main: wiring and graceful shutdown
internal/config      flags, environment, secrets, validation
internal/git         clone/fetch with token auth
internal/manifest    YAML loading and identification
internal/policy      namespace allowlist and kind rules
internal/kube        diff and server-side apply through kubectl
internal/prune       guarded deletion of objects removed from git
internal/controller  the reconcile loop and status
internal/server      /healthz, /status, /metrics, /webhook
internal/report      typed failures and per-cycle outcome
deploy/              bootstrap.yaml (RBAC) and gitops.yaml (the Deployment)
```

## License

MIT, see [LICENSE](LICENSE).