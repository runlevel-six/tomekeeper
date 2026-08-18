# Install on Kubernetes

Manifests live in `deploy/`, as a Kustomize base plus a worked example overlay.

You need a cluster with an ingress controller, a storage class that can provision
`ReadWriteOnce` volumes, and a DNS name pointed at the ingress controller.

## What gets deployed

| | |
|---|---|
| `postgres` | StatefulSet, one replica, its own volume |
| `tomekeeper-server` | the reader, behind the Ingress |
| `tomekeeper-worker` | polling, fetching, extraction |
| `tomekeeper-migrate` | a Job: applies the schema and creates the user |
| `tomekeeper-blobs` | the archive — stored pages, bodies, images |
| `tomekeeper-backups` | nightly `pg_dump` output |

## 1. Make the overlay yours

Copy the example overlay and change two things: the Ingress host and the storage
class.

```bash
cp -r deploy/overlays/example deploy/overlays/local
$EDITOR deploy/overlays/local/kustomization.yaml
```

`deploy/overlays/local` is gitignored on purpose. A hostname and a storage class
are facts about your network, and this repository is public.

If your ingress controller does not terminate TLS with a default certificate, add
a `tls` block — the overlay has the patch written out in a comment. Serving plain
HTTP instead means also setting `TOME_COOKIE_SECURE=false` in
`deploy/base/config.env`, or sign-in will appear to succeed and bounce you
straight back to the form.

## 2. Create the namespace and the secret

The namespace is not in the kustomization on purpose: if it were, `kubectl delete
-k` would take the namespace with it, and every volume in it.

```bash
kubectl create namespace tomekeeper

kubectl -n tomekeeper create secret generic tomekeeper \
  --from-literal=postgres-password="$(openssl rand -base64 24)" \
  --from-literal=session-key="$(openssl rand -base64 32)" \
  --from-literal=password='choose-a-real-password-here'
```

Three keys, three different jobs:

- **`postgres-password`** — the database password. You never type it; it is
  interpolated into `TOME_DATABASE_URL` inside the pods.
- **`session-key`** — seals session cookies. Changing it signs you out; losing it
  costs you nothing else.
- **`password`** — what you sign in with, for the user named by `TOME_USERNAME`
  (`tome` by default). Choose it yourself.

## 3. Get an image the cluster can pull

Nothing in `deploy/` builds anything. The manifests reference
`ghcr.io/runlevel-six/tomekeeper`, and that image has to exist and be pullable
before the first apply.

**From CI.** Pushing to the default branch runs the whole pipeline and, if every
job passes, publishes `:latest` and a `sha-<commit>` tag. The run summary prints
the digest.

The trap on a first publish: a container package created by a workflow is
normally **private, even on a public repository** — repository visibility governs
who can push, not who can pull. Check it after the first successful run, because
the symptom reads like a broken tag rather than a permissions problem:
`ImagePullBackOff` with `unauthorized`. Fix it once, either way round:

- Make the package public — GitHub → your packages → `tomekeeper` → Package
  settings → Change visibility. Right for an open-source project, and then no
  cluster needs credentials.
- Or keep it private and give the namespace a pull secret:

  ```bash
  kubectl -n tomekeeper create secret docker-registry ghcr \
    --docker-server=ghcr.io \
    --docker-username="$GITHUB_USER" \
    --docker-password="$GITHUB_TOKEN"   # a PAT with read:packages
  ```

  then add `imagePullSecrets` to your overlay:

  ```yaml
  patches:
    - target: {kind: Deployment}
      patch: |
        - op: add
          path: /spec/template/spec/imagePullSecrets
          value: [{name: ghcr}]
    - target: {kind: Job, name: tomekeeper-migrate}
      patch: |
        - op: add
          path: /spec/template/spec/imagePullSecrets
          value: [{name: ghcr}]
  ```

**By hand**, if you would rather not wait for a pipeline or are not using this
repository's CI:

```bash
docker build -t ghcr.io/runlevel-six/tomekeeper:latest .
echo "$GITHUB_TOKEN" | docker login ghcr.io -u "$GITHUB_USER" --password-stdin
docker push ghcr.io/runlevel-six/tomekeeper:latest
```

Point the `images:` block in your overlay at your own registry if it is not this
one.

## 4. Apply

```bash
kubectl apply -k deploy/overlays/local
```

Then watch the migration, which is the step that either works or tells you why:

```bash
kubectl -n tomekeeper wait --for=condition=complete job/tomekeeper-migrate --timeout=5m
kubectl -n tomekeeper logs job/tomekeeper-migrate
```

The server and worker will restart a few times while this runs — they cannot
start against a database with no schema, and they recover on their own once the
Job finishes. `kubectl apply` does not order resources, and gating the rollout on
a Job would mean an operator; a short crash loop is the cheaper answer.

```bash
kubectl -n tomekeeper rollout status deploy/tomekeeper-server
kubectl -n tomekeeper rollout status deploy/tomekeeper-worker
```

Then open the host from your overlay and sign in as `tome`.

## 5. Add feeds

Nothing polls until something is subscribed.

The simplest way is the web interface: **Feeds → Import subscriptions**, and choose
the OPML file your previous reader exported. No cluster access needed, and the
page reports what it added, what was already there, and anything it could not
store.

From the command line, if you would rather not use a browser:

```bash
kubectl -n tomekeeper exec -i deploy/tomekeeper-worker -- \
  tome import-opml /dev/stdin < subscriptions.opml
```

Piped rather than copied in: the runtime image is distroless, so it has no shell
and no `tar`, and `kubectl cp` needs `tar` in the container. Reading the file on
stdin sidesteps that.

Either route is idempotent — subscriptions are keyed by feed URL, so importing
the same file twice updates titles and creates nothing. That is what makes
re-running the safe way to recover from an import that stopped halfway.

## Upgrading

The publish workflow pushes `ghcr.io/runlevel-six/tomekeeper:latest` and a
`sha-<commit>` tag for every green build on the default branch. Pin the overlay
to a specific tag so that what is running is a fact rather than whatever `latest`
resolved to when the pod last restarted.

Jobs are immutable, so an upgrade that includes a migration needs the old one out
of the way first:

```bash
kubectl -n tomekeeper delete job tomekeeper-migrate --ignore-not-found
kubectl apply -k deploy/overlays/local
kubectl -n tomekeeper wait --for=condition=complete job/tomekeeper-migrate --timeout=5m
```

## Metrics

Both processes serve Prometheus metrics on port 9090. That port is deliberately
not on the Service and not routed by the Ingress: the outbound counters name
every host the archive fetches from, which is a published list of what you read.
Scrape the pods directly from inside the cluster. See
[Metrics](../reference/metrics.md).

## Backups

A CronJob dumps the database nightly to the `tomekeeper-backups` volume, keeping
seven days on rotation. That covers the index — subscriptions, read state, saved
articles — which is the part that is small and irreplaceable.

It does not cover the blob tree, which is much larger and is not something a
CronJob should be shipping around. Replicate `tomekeeper-blobs` with a tool built
for it, or accept that a total loss means re-fetching what is still online.
[Back up and restore](back-up-and-restore.md) has a recipe for copying it out — and
why the obvious `kubectl cp` does not work against a distroless image.

To restore:

```bash
# Stop writers first, so the restore is not racing the worker.
kubectl -n tomekeeper scale deploy/tomekeeper-server deploy/tomekeeper-worker --replicas=0

kubectl -n tomekeeper exec -i statefulset/postgres -- \
  sh -c 'PGPASSWORD="$POSTGRES_PASSWORD" pg_restore -h 127.0.0.1 -U tome -d tome --clean --if-exists' \
  < tome-3.dump

kubectl -n tomekeeper scale deploy/tomekeeper-server deploy/tomekeeper-worker --replicas=1
```

The dumps are on the `tomekeeper-backups` volume, which nothing mounts except the
CronJob. To get one out, run a throwaway pod against the claim:

```bash
kubectl -n tomekeeper run backup-shell --rm -it --restart=Never \
  --image=postgres:16-alpine \
  --overrides='{"spec":{"volumes":[{"name":"b","persistentVolumeClaim":{"claimName":"tomekeeper-backups"}}],"containers":[{"name":"backup-shell","image":"postgres:16-alpine","stdin":true,"tty":true,"volumeMounts":[{"name":"b","mountPath":"/backups"}]}]}}' \
  -- sh
```

## Things that will confuse you once

**Every page returns "Internal error" after an upgrade.** The image was
upgraded without the migration being run, so the new binary is querying columns
that do not exist. `/readyz` names it directly:

```bash
kubectl -n tomekeeper exec deploy/tomekeeper-server -- \
  wget -qO- http://127.0.0.1:8080/readyz
```

The fix is the upgrade sequence above: delete the old Job, re-apply, wait for the
new one. This is easy to hit because CI republishes `:latest` on every green
build and any pod restart pulls it — pin the overlay to a `sha-` tag if you would
rather upgrades never happen by surprise.

**The worker is in `CrashLoopBackOff` right after an upgrade.** Same cause. It
refuses to start against a schema older than itself rather than failing every job
it picks up and burning their retries.

**The worker sits `Pending` with "didn't match pod affinity rules."** The blob
volume is `ReadWriteOnce`, so the worker has to run on the same node as the
server, and it is waiting for a server pod to exist. It resolves itself when the
server is running. If the server is not coming up, fix that first.

**A deploy takes the site down for a few seconds.** Both Deployments use the
`Recreate` strategy, for the same reason: a surge pod on another node would sit
in `Multi-Attach` forever.

Both of these go away on `ReadWriteMany` storage: put the blob PVC on a shared
filesystem class, delete the `podAffinity` block from
`deploy/base/tomekeeper.yaml`, and set both strategies back to `RollingUpdate`.
Postgres should stay on block storage either way.

That is offered as an option, not a recommendation. Block storage is the default
here on purpose — it is what the rest of the cluster this was written for uses,
and a shared filesystem is another distributed system between the archive and its
bytes. Two workloads pinned to one node is a smaller problem than a filesystem
layer that hangs.

**No PodDisruptionBudget.** Deliberate. Both workloads are single-replica sharing
one volume, so a node drain takes the archive down whatever a budget says. One
that permitted eviction would be decoration; one that forbade it would wedge node
maintenance until someone went looking for the hung drain.

**`tome_articles{fetch_status="pending"}` stays high after the first import.**
Expected. A large OPML import queues a lot of work, and the fetcher is rate
limited on purpose. It drains.

## See also

- [Configuration](../reference/configuration.md) — every setting
- [Metrics](../reference/metrics.md)
