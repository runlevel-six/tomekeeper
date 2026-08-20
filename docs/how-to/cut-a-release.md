# How to cut a release

A release is a git tag `vX.Y.Z`. Pushing it publishes a container image under the
same string, creates a GitHub release from the changelog, and is the only way an
image gets a version number.

One identifier, four places, always the same:

| Where | What you see |
|---|---|
| git | `v0.2.0` |
| registry | `ghcr.io/runlevel-six/tomekeeper:v0.2.0` |
| the binary | `tome version` → `tomekeeper v0.2.0 (abc1234) built …` |
| the cluster | `deploy/base/kustomization.yaml` → `newTag: v0.2.0` |

CI refuses to publish a tag it has published before, so a version number always
means one set of bytes.

## Decide the number

| Bump | When | What the operator has to do |
|---|---|---|
| Patch `v0.2.0` → `v0.2.1` | Fixes only, no migration | Change the tag and apply |
| Minor `v0.2.0` → `v0.3.0` | Features, or **any** migration | Run the migration Job, then apply |

The migration rule is not a convention to remember: `scripts/check-release.sh`
compares the migration count against the previous tag and fails a patch release
that added one. It is what makes "a patch upgrade is only a tag change" true.

While the major version is `0`, a minor bump may change a default or remove a
flag. The changelog says when it does.

## 1. Write the changelog entry

Move what is under `## [Unreleased]` into a new section, newest first:

```markdown
## [v0.2.0] — 2026-09-14
```

Add the link definitions at the bottom of the file. This section becomes the
GitHub release notes verbatim, and a tag with no section fails the publish rather
than producing an empty release page — so this step is not optional.

Say plainly whether the extractor version changed. It is the one thing that wants a
follow-up command (`tome reextract`) to reach articles already archived.

## 2. Move the pin

One line in `deploy/base/kustomization.yaml`:

```yaml
images:
  - name: ghcr.io/runlevel-six/tomekeeper
    newTag: v0.2.0
```

And the same string in the five manifests that spell it out, plus `compose.yaml`.
Then let the check find what you missed:

```sh
task release:check
```

```console
ok   every image reference names v0.2.0
ok   kustomize and the manifests agree on v0.2.0
ok   v0.2.0 is a release tag
ok   CHANGELOG.md's newest release is v0.2.0
ok   migrations are consistent with a v0.1→v0.2 bump (6 → 7 files)
```

The manifests repeat the version so that the raw YAML is deployable on its own and
says what it deploys; the check is what stops the two from drifting.

## 3. Commit, tag, push

```sh
git commit -am "release: v0.2.0"
git tag -a v0.2.0 -m "v0.2.0"
git push origin master
git push origin v0.2.0
```

An annotated tag (`-a`), so the tag carries its own author and date rather than
inheriting the commit's.

Push the branch first. The tag is what triggers the publish, and a tag whose commit
is not on the remote is a release nobody else can check out.

## 4. Watch what CI does

The tag runs the full pipeline — tests, lint, fuzz, the container acceptance test —
and only then publishes. In order, the publish job:

1. Refuses the tag if that version already exists in the registry.
2. Builds, stamping `git describe` into the binary.
3. Pushes `v0.2.0`, `latest` (unless it is a prerelease), and `sha-<commit>`.
4. Runs `tome version` **inside the published image** and fails if it does not
   report the tag — which is how a broken `-ldflags` gets caught instead of turning
   up as `dev` in a log six weeks later.
5. Creates the GitHub release from the changelog section, and appends the tag and
   the digest.

The digest is in the job summary. Pin it in an overlay if you would rather not rely
on the promise that tags are never moved:

```yaml
images:
  - name: ghcr.io/runlevel-six/tomekeeper
    digest: sha256:…
```

## 5. Deploy it

Only after the image exists. See
[Install on Kubernetes](install-kubernetes.md#upgrading) for why the order matters,
but briefly:

```sh
kubectl -n tomekeeper delete job tomekeeper-migrate --ignore-not-found
kubectl apply -k deploy/overlays/local
kubectl -n tomekeeper wait --for=condition=complete job/tomekeeper-migrate --timeout=5m
```

Because the pin changed, the apply now changes the Deployments' specs and rolls
them itself — there is nothing to restart by hand, which is the failure this whole
arrangement exists to prevent.

## What the other tags mean

| Tag | Points at | Use it for |
|---|---|---|
| `vX.Y.Z` | one release, forever | deployments |
| `latest` | the newest non-prerelease release | trying the tool out |
| `edge` | the tip of the default branch | testing something before it is released |
| `sha-<commit>` | one build, forever | bisecting |

There are deliberately no moving `0.2` or `0` aliases. A tag that moves underneath
a running deployment is what took this archive down once already, and while the
major version is `0` a minor bump is allowed to break things — so `:0` would be a
promise this project cannot keep.

## Prereleases

Tag `v0.3.0-rc.1`. It publishes under its own name and is marked as a prerelease on
GitHub; `latest` does not move. Everything else is identical.

## See also

- [CHANGELOG.md](https://github.com/runlevel-six/tomekeeper/blob/master/CHANGELOG.md)
  — what the version numbers promise
- [Install on Kubernetes](install-kubernetes.md) — the deploy sequence and why it
  is that sequence
- [CLI](../reference/cli.md#tome-version) — what `tome version` reports and where
  it comes from
