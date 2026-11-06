# Preparing CI for merge queues

The workflows and area-maintainer bot support merge queues, but these changes
do not enable a queue or change GitHub branch protection settings.

## Checks to require at cutover

Keep the existing `DCO` and `Build-PR` requirements and add:

| Check | Coverage |
| --- | --- |
| `CI Summary` | Go cache preparation, lint/generated code, base/Fedora/Ubuntu image builds, `Test-PR` (unit and CRD tests), every `e2e` matrix entry including conformance, both upgrade gateway modes, and dual-stack conversion |
| `DPU CI Summary` | Both DPU lanes, including traffic-flow, Uplink and DPU host-node tests |

The summaries use `always()` and inspect dependency results explicitly: failed,
cancelled and unexpectedly skipped jobs fail the check. A matrix dependency must
succeed across all entries. When adding a separate test job, also add it to the
summary's `needs` list. Diagnostic coredump processing is not a merge requirement.

Ordinary docs-only PRs retain their test exemption. The DPU workflow now starts a
small changed-file job and reports its summary even for those PRs, rather than
skipping the entire workflow. Queue runs execute all these test lanes. Scheduled
main CI retains its existing exclusion of upgrade and dual-stack conversion tests.

Leave the summaries optional during preparation. On branches without a queue,
the area-maintainer bot ignores the two summaries but continues to check their
underlying jobs. Once the queue is enabled it also checks the summaries. GitHub
must require them explicitly to enforce them on queue candidates and manual merges.

Performance benchmarks, license-header checks and script tests are separate
workflows and are not covered by these two summaries. If additional checks become
required, verify that they report on both PR and merge-group commits.

## Base and candidate revisions

For ordinary PRs, base-image caching and base-branch checkouts keep their existing
behavior. For `merge_group`, the base image is built from the event's `base_sha`;
branch-tagged images are not reused because they may represent a different commit.
Upgrade tests check out that same base SHA. Default candidate checkouts use the
merge-group revision, including the PRs ahead of the entry in the queue.

## Area-maintainer commands and retries

`/area-maintainer-approved` continues to check the requester's file scope, prevent
self-approval and bind an automatic follow-up to the authorized PR head SHA.

The bot queries the target branch's queue. Without a queue it merges directly;
with a queue it calls `enqueuePullRequest` with `expectedHeadOid`. GitHub still
enforces native approvals and branch protection. Queue lookup or enqueue errors
never cause a fallback to direct merging. An already queued PR is left alone.

After removal from the queue, an authorized area maintainer can post the command
again after investigating the failure. The bot checks queue timeline events so
an old waiting comment cannot automatically requeue a PR after CI failure or
deliberate removal. Repository maintainers can use GitHub's queue controls.

`/retest` and `/retest-failed` continue to target the initial `pull_request` runs.
They do not requeue a PR or retry its merge-group run. Requeueing creates a new
candidate that must satisfy all required checks again.

## Validation and activation

Before enabling the production queue:

1. Run `node --test .github/scripts/*.test.js` and validate the changed workflows
   with actionlint. Observe successful summaries for ordinary code and docs PRs.
2. On an isolated staging branch, configure the same review/check requirements
   and a merge queue. Verify the bot's queue lookup using its installation token,
   initial enqueue, already-queued behavior, removal, and an explicit retry.
3. Confirm that the installed DCO app reports `DCO` on the actual merge-group SHA.
   Upstream source support alone is not proof that the installed service works.
4. Confirm every required E2E/conformance lane executes on a queue candidate.
   Exercise a failed lane and a failed prerequisite: neither summary may pass.
   Confirm `/retest-failed` on an initial PR run reruns the dependent summary and
   updates it after recovery. Local evaluator tests do not prove GitHub rerun behavior.
5. Confirm stale approvals/new commits and queue/API failures cannot enqueue or
   directly merge an unauthorized revision. Keep existing CODEOWNER and review rules.

A disabled ruleset targeting exactly `master` can stage the queue and summary
requirements alongside existing branch protection. Select the queue merge method,
concurrency, merge limits, timeout and admin bypass policy before activation.
Keep "Only merge non-failing pull requests" enabled. Account for image builds and
runner waits when choosing the timeout. Do not grant the merge bot queue bypass.

Refresh CI for open PRs that predate the summary jobs, then activate the ruleset.
The bot detects the enabled queue without another repository variable. Disabling
this additive ruleset restores the existing branch protection and direct-merge
path; inspect any remaining queue entries during rollback.

References:

- [Managing merge queues](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-a-merge-queue)
- [Required check behavior](https://docs.github.com/en/pull-requests/how-tos/merge-and-close-pull-requests/troubleshooting-required-status-checks)
- [Ruleset layering and enforcement](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/about-rulesets)
