# Release matrix

## Following upstream releases

`follow-upstream-release.yml` checks for the latest upstream release once a day
at 04:17 UTC. Use its **Run workflow** button to check sooner; select `dry_run`
to merge and verify without pushing or publishing.

Merge conflicts and verification failures fail the run and open one issue per
release and failure type. Subsequent failures reference the open issue in the
run log without adding notification-generating comments. Full failure details
remain in each run log. Keep fork documentation exceptions beside the existing
`docs/*` exceptions in `.gitignore`, so upstream additions at the end of the file
can merge without conflicting with fork-only additions.

GitHub's built-in Actions notifications are controlled by the recipient, not by
workflow YAML. To silence successful runs, open
[Settings → Notifications](https://github.com/settings/notifications), then
**System → Actions**, select **Only notify for failed workflows**, and save.
This is an account-wide preference, not a per-repository switch. See
[GitHub's notification documentation](https://docs.github.com/en/subscriptions-and-notifications/how-tos/managing-github-actions-notifications).

The merge verification enables the `unit` build tag and runs the upstream
Anthropic stream framing/integrity and gateway protocol regressions alongside
the fork's replay, synchronous streaming and keepalive checks. Keep these tests
enabled when adapting fork patches to upstream parser changes.

Deployed header-wait and JSON-validation patches are committed on `stable`.
Verification also covers code-less encrypted rejection recovery, the independent
400 recovery switch, tool-history preservation and the same-account retry limit.
The workflow ensures Redis is available so the real HTTP/Redis recovery tests run
instead of being skipped. New upstream tags are merged into this maintained
branch; release archives and images are built from the verified merged commit.

Run the offline merge, verification, publication and notification checks with:

```bash
bash .github/release-tools/test_follow_upstream.sh
```

## Building release artifacts

The release workflow builds the frontend once, then runs each configured Go target on its own Linux runner. `CGO_ENABLED=0` permits cross-compilation. The target matrix is read from `.goreleaser.yaml`, including its exclusions; simple releases select Linux amd64 only.

Each build uses GoReleaser OSS in snapshot mode with the selected release version and one target. Archive naming, bundled files, Go flags and release templates remain in the existing GoReleaser configurations. Every archive is accompanied by its source commit, target, version and SHA256. The publishing job verifies the complete matrix before building images or publishing. It uses GoReleaser's `extra_files` support to publish existing archives and checksums, with builds disabled. No Pro license is needed.

Go caches are isolated by target and refreshed on each source commit, with fallback to the preceding target cache. Save uses the original restore key, even if a build hook changes `go.sum`. Matrix jobs upload uniquely named artifacts. The publishing job extracts only the regular Linux binary from each verified archive and restores its executable permission before constructing Docker contexts. QEMU remains limited to runtime-image instructions. DockerHub images are omitted when its credentials are absent; GHCR is always retained. Simple mode still publishes only the amd64 GHCR image and the simple release description.

All build jobs use the commit resolved by `prepare`, including a manual release's selected tag. Helper scripts come from the workflow revision and are passed as a run-local artifact, so older application tags do not need to contain the new scripts. The workflow serializes release runs to prevent simultaneous updates to moving image tags.

## Validate without publication

From a branch containing this workflow:

```bash
gh workflow run release.yml --ref <branch> \
  -f tag=<branch> -f dry_run=true -f simple_release=false
```

A dry run builds all selected archives and both runtime images, verifies artifact provenance and produces the final checksum file. It exports images locally as OCI archives instead of pushing them. It skips registry logins, GitHub Release publication, DockerHub description updates, Telegram notifications and VERSION synchronization. Test the simple path separately with `simple_release=true`.

Dry-run artifacts are available in the Actions run, including `release-dry-run-report`. Compare job start/end times, GoReleaser's build duration and cache restore results. Do not present an initial cold-cache run as a warmed-cache benchmark; publishing network time is not measured by dry runs.

Helper checks:

```bash
python -m pip install -r .github/release-tools/requirements-release.txt
python -m unittest discover -s .github/release-tools -p 'test_release_matrix.py'
bash -n .github/release-tools/release-images.sh
```
