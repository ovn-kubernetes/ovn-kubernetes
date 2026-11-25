const assert = require('node:assert/strict');
const fs = require('node:fs');
const { test } = require('node:test');
const areaMerge = require('./area-merge');

const botLogin = process.env.BOT_LOGIN || 'ovn-kubernetes-merge-bot[bot]';

/**
 * Mock GitHub for an open PR whose head commit "approved-sha" an area
 * maintainer has approved with /area-maintainer-approved. Tests change the
 * returned state to describe a scenario, then deliver an event with run().
 */
function fixture(t) {
  t.mock.method(fs, 'readFileSync', () =>
    '# Test area (Area Maintainer: @maintainer)\n/test/e2e/ @maintainer\n');
  const pr = { number: 6981, state: 'open', user: { login: 'author' }, head: { sha: 'approved-sha' } };
  const command = { user: { login: 'maintainer', type: 'User' }, body: '/area-maintainer-approved' };
  const comments = [command];
  const checks = [{ name: 'e2e', status: 'completed', conclusion: 'success' }];
  // The GitHub Actions workflow runs of the repository; each owns one check suite.
  const workflowRuns = [];
  // GitHub returns state "pending" with total_count 0 when a commit has no legacy statuses.
  const status = { state: 'pending', total_count: 0, statuses: [] };
  const github = {
    rest: {
      actions: {
        listWorkflowRunsForRepo() {},
      },
      repos: {
        getCombinedStatusForRef: async () => ({ data: status }),
      },
      pulls: {
        get: async () => ({ data: pr }),
        listFiles: async () => ({ data: [{ filename: 'test/e2e/kubevirt.go' }] }),
        merge: t.mock.fn(async () => ({ data: { merged: true } })),
      },
      issues: {
        listComments: async () => ({ data: comments }),
        createComment: t.mock.fn(async ({ body }) => {
          const comment = { user: { login: botLogin, type: 'Bot' }, body };
          comments.push(comment);
          return { data: comment };
        }),
      },
      checks: {
        listForRef: async () => ({ data: { check_runs: checks, total_count: checks.length } }),
      },
    },
    paginate: async (endpoint, query) => workflowRuns.filter(run => run.head_sha === query.head_sha),
  };
  // A core.setFailed call fails the test unless the test replaces it.
  const core = { info() {}, setFailed: message => assert.fail(message) };
  const run = eventName => areaMerge({
    github,
    core,
    context: {
      eventName,
      repo: { owner: 'ovn-kubernetes', repo: 'ovn-kubernetes' },
      payload: { comment: command, issue: { number: pr.number } },
    },
  });
  return { pr, command, comments, checks, workflowRuns, status, github, core, merge: github.rest.pulls.merge, run };
}

/** Report one legacy commit status in the given state, as a non-Actions CI system would. */
function setCommitStatus(f, state) {
  Object.assign(f.status, { state, total_count: 1, statuses: [{ context: 'external-ci', state }] });
}

/**
 * Report two Build-PR check runs, given as [workflow, conclusion] with the
 * older one first. Each belongs to its own workflow run; ovn-ci and
 * performance-test both have a job of that name.
 */
function setBuildChecks(f, ...runs) {
  const workflowIds = { 'ovn-ci': 10, 'performance-test': 20 };
  runs.forEach(([workflow, conclusion], index) => {
    const suite = index + 1;
    f.workflowRuns.push({ head_sha: f.pr.head.sha, check_suite_id: suite, workflow_id: workflowIds[workflow] });
    f.checks.push({
      name: 'Build-PR', check_suite: { id: suite }, status: 'completed', conclusion,
      started_at: `2026-11-25T10:0${suite}:00Z`,
    });
  });
}

// What an approval does with the CI results reported for the approved head:
// how many merges it attempts and what the bot replies on the PR.
for (const [outcome, setup, merges, reply] of [
  ['merges when check runs and commit statuses passed',
    f => setCommitStatus(f, 'success'), 1, /^PR merged by area maintainer @maintainer/],
  ['waits for a pending commit status',
    f => setCommitStatus(f, 'pending'), 0, /^<!-- area-merge-sha:approved-sha -->.*- `external-ci`$/s],
  ['is blocked by a failed check run',
    f => { f.checks[0].conclusion = 'failure'; }, 0, /CI checks have failed:\n- `e2e`\n/],
  ['is blocked by a failed commit status',
    f => setCommitStatus(f, 'failure'), 0, /CI checks have failed:\n- `external-ci`\n/],
  ['merges when check runs passed and there are no commit statuses',
    () => {}, 1, /^PR merged by area maintainer @maintainer/],
  ['waits when CI has not reported anything yet',
    f => { f.checks.length = 0; }, 0, /^<!-- area-merge-sha:approved-sha -->.*- `\(no CI results reported yet\)`$/s],
  ['waits when the only check run is the bot\'s own',
    f => { f.checks[0].name = 'area-merge'; }, 0, /^<!-- area-merge-sha:approved-sha -->.*- `\(no CI results reported yet\)`$/s],
  ['merges when a later run of the same workflow replaced a cancelled check',
    f => setBuildChecks(f, ['ovn-ci', 'cancelled'], ['ovn-ci', 'success']),
    1, /^PR merged by area maintainer @maintainer/],
  ['is blocked by a failed check even when the same job of another workflow passed later',
    f => setBuildChecks(f, ['ovn-ci', 'failure'], ['performance-test', 'success']),
    0, /CI checks have failed:\n- `Build-PR`\n/],
  ['reports a job name once when the job failed in two workflows',
    f => setBuildChecks(f, ['ovn-ci', 'failure'], ['performance-test', 'failure']),
    0, /CI checks have failed:\n- `Build-PR`\n\n/],
]) {
  test(`approval ${outcome}`, async t => {
    const f = fixture(t);
    setup(f);
    await f.run('issue_comment');
    assert.equal(f.merge.mock.callCount(), merges);
    assert.match(f.comments.at(-1).body, reply);
  });
}
