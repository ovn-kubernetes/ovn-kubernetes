const assert = require('node:assert/strict');
const fs = require('node:fs');
const { test } = require('node:test');
const areaMerge = require('./area-merge');

const botLogin = process.env.BOT_LOGIN || 'ovn-kubernetes-merge-bot[bot]';

/**
 * Mock GitHub for an open PR whose head commit "approved-sha" an area
 * maintainer has approved with /area-maintainer-approved. With `waiting`, the
 * bot has already answered with its waiting marker for that head. Tests change
 * the returned state to describe a scenario, then deliver an event with run().
 */
function fixture(t, { waiting = false } = {}) {
  t.mock.method(fs, 'readFileSync', () =>
    '# Test area (Area Maintainer: @maintainer)\n/test/e2e/ @maintainer\n');
  const headRepository = { id: 1234, owner: { login: 'author' } };
  const pr = {
    number: 6981, state: 'open', user: { login: 'author' },
    head: { sha: 'approved-sha', ref: 'test-branch', repo: headRepository },
  };
  const command = { user: { login: 'maintainer', type: 'User' }, body: '/area-maintainer-approved' };
  const comments = [command];
  if (waiting) {
    comments.push({ user: { login: botLogin, type: 'Bot' }, body: '<!-- area-merge-sha:approved-sha -->' });
  }
  const checks = [{ name: 'e2e', status: 'completed', conclusion: 'success' }];
  // The GitHub Actions workflow runs of the repository; each owns one check suite.
  const workflowRuns = [];
  // GitHub returns state "pending" with total_count 0 when a commit has no legacy statuses.
  const status = { state: 'pending', total_count: 0, statuses: [] };
  // The CI completion event for the approved head.
  const source = {
    head_sha: pr.head.sha, pull_requests: [{ number: pr.number }],
    event: 'pull_request', head_repository: headRepository, head_branch: pr.head.ref,
  };
  // Commit associations can include PRs whose heads have since advanced.
  const associatedSha = pr.head.sha;
  const associatedPRs = [pr];
  const branchPRs = [pr];
  const github = {
    rest: {
      actions: {
        listWorkflowRunsForRepo() {},
      },
      repos: {
        listPullRequestsAssociatedWithCommit() {},
        getCombinedStatusForRef: async () => ({ data: status }),
      },
      pulls: {
        list() {},
        get: async ({ pull_number }) => ({
          data: [pr, ...associatedPRs, ...branchPRs].find(p => p.number === pull_number),
        }),
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
    paginate: async (endpoint, query) => {
      if (endpoint === github.rest.actions.listWorkflowRunsForRepo) {
        return workflowRuns.filter(run => run.head_sha === query.head_sha);
      }
      // Associations are tied to the queried commit, not the PRs' current heads.
      if (endpoint === github.rest.repos.listPullRequestsAssociatedWithCommit) {
        return query.commit_sha === associatedSha ? associatedPRs : [];
      }
      return branchPRs.filter(p => `${p.head.repo.owner.login}:${p.head.ref}` === query.head);
    },
  };
  // A core.setFailed call fails the test unless the test replaces it.
  const core = { info() {}, setFailed: message => assert.fail(message) };
  const run = eventName => areaMerge({
    github,
    core,
    context: {
      eventName,
      repo: { owner: 'ovn-kubernetes', repo: 'ovn-kubernetes' },
      // Like a real delivery, the payload carries only the triggering event.
      payload: eventName === 'issue_comment'
        ? { comment: command, issue: { number: pr.number } }
        : { [eventName]: source },
    },
  });
  return {
    pr, command, comments, checks, workflowRuns, status, source, associatedPRs, branchPRs, github, core,
    merge: github.rest.pulls.merge, run,
  };
}

/** Report one legacy commit status in the given state, as a non-Actions CI system would. */
function setCommitStatus(f, state) {
  Object.assign(f.status, { state, total_count: 1, statuses: [{ context: 'external-ci', state }] });
}

/**
 * Report a run of the ovn-ci workflow for the approved head, created and
 * started after the runs reported before it. Its check runs belong to check
 * suite 9.
 */
function addWorkflowRun(f, run) {
  const id = f.workflowRuns.length + 1;
  f.workflowRuns.push({
    id, run_started_at: `2026-11-25T10:1${id}:00Z`, head_sha: f.pr.head.sha, check_suite_id: 9, workflow_id: 10,
    name: 'ovn-ci', event: 'pull_request', status: 'completed', ...run,
  });
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
    addWorkflowRun(f, { check_suite_id: suite, workflow_id: workflowIds[workflow], name: workflow, conclusion });
    f.checks.push({
      name: 'Build-PR', check_suite: { id: suite }, status: 'completed', conclusion,
      started_at: `2026-11-25T10:0${suite}:00Z`,
    });
  });
}

/** Check runs published by the repository's label and review workflows. */
function helperChecks(status, conclusion) {
  return ['labeler', 'approval-labeler', 'review'].map(name => ({
    name, app: { slug: 'github-actions' }, status, conclusion,
  }));
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
  ['waits for a queued workflow that has not reported any check yet',
    f => addWorkflowRun(f, { status: 'queued' }),
    0, /^<!-- area-merge-sha:approved-sha -->.*- `ovn-ci \(workflow\)`$/s],
  ['lists only the pending check of a workflow that is still running', f => {
    Object.assign(f.checks[0], { status: 'in_progress', check_suite: { id: 9 } });
    addWorkflowRun(f, { status: 'in_progress' });
  }, 0, /Pending checks:\n- `e2e`$/],
  ['merges while a workflow run for another event has not completed',
    f => addWorkflowRun(f, { name: 'Pull Request Labeler', event: 'pull_request_target', status: 'in_progress' }),
    1, /^PR merged by area maintainer @maintainer/],
  ['waits for a fork workflow that awaits maintainer approval',
    f => addWorkflowRun(f, { conclusion: 'action_required' }),
    0, /^<!-- area-merge-sha:approved-sha -->.*- `ovn-ci \(workflow\)`$/s],
  ['is blocked by a workflow that failed before it created a job',
    f => addWorkflowRun(f, { conclusion: 'startup_failure' }),
    0, /CI checks have failed:\n- `ovn-ci \(workflow\)`\n/],
  ['lists only the failed check of a workflow that failed', f => {
    Object.assign(f.checks[0], { conclusion: 'failure', check_suite: { id: 9 } });
    addWorkflowRun(f, { conclusion: 'failure' });
  }, 0, /CI checks have failed:\n- `e2e`\n\n/],
  ['merges when a rerun of a workflow that failed without a job passed', f => {
    addWorkflowRun(f, { check_suite_id: 8, conclusion: 'startup_failure' });
    addWorkflowRun(f, { conclusion: 'success' });
  }, 1, /^PR merged by area maintainer @maintainer/],
  // A rerun keeps the ID of its run, so the passing run here has the lower ID.
  ['merges when an older run of a workflow was rerun and passed after a newer one failed', f => {
    addWorkflowRun(f, { check_suite_id: 8, conclusion: 'success', run_started_at: '2026-11-25T11:00:00Z' });
    addWorkflowRun(f, { conclusion: 'startup_failure' });
  }, 1, /^PR merged by area maintainer @maintainer/],
  ['waits when only helper checks have reported',
    f => { f.checks.splice(0, 1, ...helperChecks('completed', 'success')); },
    0, /^<!-- area-merge-sha:approved-sha -->.*- `\(no CI results reported yet\)`$/s],
  ['is blocked by another App\'s check even when a same-named helper is newer', f => {
    f.checks.push(
      { name: 'review', app: { slug: 'external-ci' }, status: 'completed', conclusion: 'failure', started_at: '2026-11-25T10:00:00Z' },
      { name: 'review', app: { slug: 'github-actions' }, status: 'completed', conclusion: 'success', started_at: '2026-11-25T10:01:00Z' },
    );
  }, 0, /CI checks have failed:\n- `review`\n/],
]) {
  test(`approval ${outcome}`, async t => {
    const f = fixture(t);
    setup(f);
    await f.run('issue_comment');
    assert.equal(f.merge.mock.callCount(), merges);
    assert.match(f.comments.at(-1).body, reply);
  });
}

for (const [outcome, status, conclusion] of [
  ['pending', 'in_progress', null],
  ['failed', 'completed', 'failure'],
]) {
  test(`last CI completion merges while helper checks are ${outcome}`, async t => {
    const f = fixture(t);
    f.checks[0].status = 'in_progress';
    f.checks.push(...helperChecks(status, conclusion));

    await f.run('issue_comment');
    assert.equal(f.merge.mock.callCount(), 0);
    assert.match(f.comments.at(-1).body, /^<!-- area-merge-sha:approved-sha -->/);

    f.checks[0].status = 'completed';
    await f.run('workflow_run');
    assert.equal(f.merge.mock.callCount(), 1);
    assert.equal(f.merge.mock.calls[0].arguments[0].sha, 'approved-sha');
  });
}

/** Run callbacks just before and just after the bot's waiting comment becomes visible. */
function aroundWaitingComment(t, f, { before = () => {}, after = () => {} }) {
  const createComment = f.github.rest.issues.createComment;
  t.mock.method(f.github.rest.issues, 'createComment', async params => {
    const isWaitingComment = params.body.startsWith('<!-- area-merge-sha:');
    if (isWaitingComment) await before();
    const result = await createComment(params);
    if (isWaitingComment) await after();
    return result;
  });
}

test('approval merges when CI completes just before the waiting marker is published', async t => {
  const f = fixture(t);
  f.checks[0].status = 'in_progress';
  aroundWaitingComment(t, f, {
    // The last completion event finds no marker yet and leaves the PR alone.
    before: async () => {
      f.checks[0].status = 'completed';
      await f.run('check_suite');
      assert.equal(f.merge.mock.callCount(), 0);
    },
  });

  await f.run('issue_comment');
  assert.equal(f.merge.mock.callCount(), 1);
  assert.equal(f.merge.mock.calls[0].arguments[0].sha, 'approved-sha');
});

test('approval tolerates a completion merging just after the waiting marker is published', async t => {
  const f = fixture(t);
  f.checks[0].status = 'in_progress';
  // GitHub rejects a second merge of the same PR.
  f.merge.mock.mockImplementation(async () => {
    if (f.pr.merged) throw new Error('Pull Request is not mergeable');
    Object.assign(f.pr, { state: 'closed', merged: true });
    return { data: { merged: true } };
  });
  aroundWaitingComment(t, f, {
    after: async () => {
      f.checks[0].status = 'completed';
      await f.run('check_suite');
      assert.equal(f.merge.mock.callCount(), 1);
    },
  });

  await f.run('issue_comment');
  assert.equal(f.merge.mock.callCount(), 2);
  assert.match(f.comments.at(-1).body, /^PR merged by area maintainer @maintainer/);
});

test('a merge rejected for any other reason is still reported', async t => {
  const f = fixture(t);
  const setFailed = t.mock.method(f.core, 'setFailed', () => {});
  f.merge.mock.mockImplementation(async () => { throw new Error('merge rejected'); });

  await f.run('issue_comment');
  assert.equal(setFailed.mock.callCount(), 1);
  assert.match(f.comments.at(-1).body, /^Failed to merge this PR: `merge rejected`/);
});

// Completion events do not list fork PRs, and GitHub's commit lookup can miss them too.
for (const [lookup, setup] of [
  ['head SHA', f => { f.branchPRs.length = 0; }],
  ['source branch', f => { f.associatedPRs.length = 0; }],
  ['source branch despite stale commit associations', f => {
    f.associatedPRs.splice(0, 1, {
      ...f.pr,
      number: f.pr.number + 1,
      head: { ...f.pr.head, sha: 'newer-sha' },
    });
  }],
]) {
  test(`fork approval waits for CI, then a workflow completion finds the PR by ${lookup} and merges it`, async t => {
    const f = fixture(t);
    f.source.pull_requests = [];
    setup(f);
    f.checks[0].status = 'in_progress';
    await f.run('issue_comment');
    assert.equal(f.merge.mock.callCount(), 0);
    assert.match(f.comments.at(-1).body, /^<!-- area-merge-sha:approved-sha -->/);

    f.checks[0].status = 'completed';
    await f.run('workflow_run');
    assert.equal(f.merge.mock.callCount(), 1);
    assert.equal(f.merge.mock.calls[0].arguments[0].pull_number, f.pr.number);
    assert.equal(f.merge.mock.calls[0].arguments[0].sha, 'approved-sha');
  });
}

// A completion only finishes an approval that the bot recorded for the PR's current head.
for (const [reason, setup] of [
  ['there is no waiting marker', f => { f.comments.pop(); }],
  ['the waiting marker is for an older head', f => { f.comments[1].body = '<!-- area-merge-sha:old-sha -->'; }],
  ['a human posted the waiting marker', f => { f.comments[1].user = f.command.user; }],
  ['the completed run is for an older head', f => { f.source.head_sha = 'old-sha'; }],
]) {
  test(`workflow completion does not merge when ${reason}`, async t => {
    const f = fixture(t, { waiting: true });
    setup(f);
    await f.run('workflow_run');
    assert.equal(f.merge.mock.callCount(), 0);
    assert.equal(f.github.rest.issues.createComment.mock.callCount(), 0);
  });
}

test('workflow completion rechecks that the approver is an area maintainer for the changed files', async t => {
  const f = fixture(t, { waiting: true });
  f.command.user.login = 'other-reviewer';
  await f.run('workflow_run');
  assert.equal(f.merge.mock.callCount(), 0);
  assert.match(f.comments.at(-1).body, /not listed as an Area Maintainer/);
});
