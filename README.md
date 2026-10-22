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
- Reports status as JSON (`/status`) and Prometheus metrics (`/metrics`),
  including the rollout [health](#health) of each Deployment.

It does not build images. A typical flow is: CI builds the image, CI commits the
new tag into the manifest repo, and this tool deploys it.

## Safety model

Every one of these is on by default, and you opt out explicitly:

| Safeguard | Behavior |
|---|---|
| **Dry-run first** | `-dry-run` defaults to **true**: it logs the diff and changes nothing until you pass `-dry-run=false`. |
| **Namespace allowlist** | Only namespaces in `-namespaces` may be touched. Cluster-scoped kinds (ClusterRole, CRD, ...) are rejected. A `Namespace` object is allowed only if it is allowlisted. |
| **A bad commit never applies** | One policy violation or unparseable file rejects the whole revision. Nothing is half-applied, and the last good state stays. |
| **No plaintext Secrets** | `kind: Secret` is rejected unless you pass `-allow-plaintext-secrets`. Commit an encrypted SealedSecret instead (see [Secrets](#secrets)). |
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
Then uncomment `GITOPS_GIT_TOKEN_FILE` in `deploy/config.yaml`.

**4. Edit the settings in `deploy/config.yaml`** (the `gitops-config` ConfigMap):

- `GITOPS_REPO`, `GITOPS_BRANCH`, `GITOPS_PATH`: where your manifests live.
- `GITOPS_NAMESPACES`: every namespace it may manage (must match `bootstrap.yaml`).
- Leave `GITOPS_DRY_RUN: "true"` for now.

In `deploy/gitops.yaml`, check that `image:` is a pinned tag such as
`mateosxh/gitops:v0.1.0` or `sha-<short>`. Prefer a pinned tag to `latest`, so
you know exactly what is running. Files named `deploy/*.local.yaml` are
gitignored, for copies holding your own values.

**5. Deploy and watch.** The ConfigMap must exist before the Deployment starts:

```bash
kubectl apply -f deploy/config.yaml
kubectl apply -f deploy/gitops.yaml
kubectl -n gitops-system logs deploy/gitops -f
```

You should see `reconciled ... sync=OutOfSync` plus the diffs it would apply.
When they look right, set `GITOPS_DRY_RUN: "false"` in `deploy/config.yaml`,
apply it, and restart the controller (it reads its settings only at start):

```bash
kubectl apply -f deploy/config.yaml
kubectl -n gitops-system rollout restart deploy/gitops
```

### Upgrading

Every setting change needs `kubectl apply -f deploy/config.yaml` followed by
the `rollout restart` above. To change the version, set the new tag on the
`image:` line of `deploy/gitops.yaml`, then:

```bash
kubectl apply -f deploy/gitops.yaml
kubectl -n gitops-system rollout status deploy/gitops
```

Each GitHub release attaches `gitops.yaml` with its image tag already set, plus
`config.yaml` and `bootstrap.yaml`.

From a clone of this repo, `scripts/deploy.sh` does the same for the image CI
built for the current commit: it refuses unless the `sha-<short>` tag exists on
Docker Hub, sets the `image:` line, applies the file and waits for the rollout.
`scripts/deploy.sh sha-1a2b3c4` deploys (or rolls back to) a specific tag.
Commit `deploy/gitops.yaml` afterwards so git records what is running.

### Requirements for the cluster

- Images are published for `linux/amd64` and `linux/arm64` (e.g. Raspberry Pi
  or Graviton nodes); the runtime picks the right one.
- The image bundles `kubectl` v1.36.5; keep it within one minor version of your
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

## Secrets

Commit secrets encrypted, as [Sealed Secrets](https://github.com/bitnami-labs/sealed-secrets)
(`apiVersion: bitnami.com/v1alpha1`, `kind: SealedSecret`). This controller
applies the encrypted object like any other namespaced kind; the Sealed
Secrets controller in the cluster decrypts it into a normal Secret. This
controller never sees the plaintext and has no access to Secrets.

**1. Install Sealed Secrets (once, as a cluster admin).** Pin a release:

```bash
kubectl apply -f https://github.com/bitnami-labs/sealed-secrets/releases/download/v0.40.0/controller.yaml
```

Install the matching `kubeseal` CLI from the same release page.

**2. Allow the kind.** `deploy/bootstrap.yaml` already grants the
controller `get, list, create, patch, update` on `sealedsecrets` in `bitnami.com`.
Never grant it `secrets`.

**3. Seal and commit.** The plaintext only ever goes through a pipe:

```bash
read -rs DB_PASSWORD
kubectl create secret generic db -n demo --from-literal=password="$DB_PASSWORD" \
  --dry-run=client -o yaml | kubeseal --format yaml > manifests/db-sealed.yaml
unset DB_PASSWORD
git add manifests/db-sealed.yaml && git commit -m "add db secret" && git push
```

The next cycle logs `applied`, and the Secret `db` appears in `demo`. A
SealedSecret can only be decrypted in the namespace (and under the name) it
was sealed for.

**Removing one.** Deleting the file from git does **not** delete the
SealedSecret unless you add `sealedsecrets.bitnami.com` to `-prune-kinds` and
`delete` to its Role rule. Otherwise delete it by hand:
`kubectl -n demo delete sealedsecret db` (its Secret goes with it).

**Back up the sealing key.** If the Sealed Secrets controller's key in
`kube-system` is lost, nothing in git can be decrypted again.

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
| `GET /status` | JSON: revision, sync state (`Synced`, `OutOfSync`, `Unknown`), last error and its kind, counters, `health` and the `unhealthy` resources |
| `GET /metrics` | Prometheus text format |
| `POST /webhook` | Trigger an immediate sync (signed) |

Metrics: `gitops_reconcile_total`, `gitops_applies_total`, `gitops_pruned_total`,
`gitops_resources`, `gitops_out_of_sync`, `gitops_unhealthy_resources`,
`gitops_last_success_timestamp_seconds`,
and `gitops_reconcile_failures_total{kind="..."}`.

### Health

After every successful apply it reads each Deployment in the repo with
`kubectl get` and reports one of:

| State | Meaning |
|---|---|
| `Healthy` | The current spec is observed, and all replicas are updated and available. |
| `Progressing` | A rollout is still converging, with no failure yet. |
| `Degraded` | The rollout failed: `Progressing=False` (e.g. `ProgressDeadlineExceeded`) or `ReplicaFailure=True`. |
| `Unknown` | The object could not be read (missing, or `kubectl` failed). |

`/status` shows the worst state as `health` and lists every non-healthy
Deployment with a short reason under `unhealthy`. A Deployment that stays
`Degraded` for more than one cycle logs a `WARN` line each cycle.

Health is **reported only**. It never fails a cycle, changes the sync state
or rolls anything back. To roll back, `git revert` the commit and the next
cycle applies the previous state. It needs only the `get` verb on
deployments, which `deploy/bootstrap.yaml` already grants. Note that a
Deployment with a bad image stays `Progressing` until its
`progressDeadlineSeconds` (default 600s) has passed, and only then becomes `Degraded`.

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
- **Secrets only as SealedSecrets.** Plaintext Secrets are rejected on
  purpose; see [Secrets](#secrets). SOPS is not supported (the controller
  never decrypts anything). SealedSecrets are not pruned unless you add
  `sealedsecrets.bitnami.com` to `-prune-kinds`.
- **Identity fields only.** Each document is parsed with a real YAML parser
  (`go.yaml.in/yaml/v3`), but only `apiVersion`, `kind`, `metadata.name` and
  `metadata.namespace` are read; the document is sent to kubectl exactly as
  written. A document that cannot be identified (invalid YAML, not a mapping,
  missing fields, content on a `---` line) is an error naming the file and
  document number, never a silent skip.
- **Single cluster, single repo path per instance.** Run several instances,
  each with its own `-owner`, for several sources.
- **Health covers Deployments only**, is reported but never acted on, and
  there is no UI.

## Development

```bash
go test -race ./...      # unit tests
go vet ./... && gofmt -l .
```

Requirements: Go 1.24+, `git`, and `kubectl` on the PATH for a local run. The
only dependency outside the standard library is `go.yaml.in/yaml/v3`.

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
deploy/              bootstrap.yaml (RBAC), config.yaml (settings), gitops.yaml (the Deployment)
scripts/deploy.sh    deploy the CI-built image for the current commit
```

## License

MIT, see [LICENSE](LICENSE).