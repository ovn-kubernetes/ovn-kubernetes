const assert = require('node:assert/strict');
const fs = require('node:fs');
const { test } = require('node:test');
const areaMerge = require('./area-merge');

const botLogin = process.env.BOT_LOGIN || 'ovn-kubernetes-merge-bot[bot]';
const waitingComment = sha => ({ user: { login: botLogin, type: 'Bot' }, body: `<!-- area-merge-sha:${sha} -->` });

/** A successful check run of a GitHub Actions job. */
const actionsCheck = name =>
  ({ name, app: { slug: 'github-actions' }, status: 'completed', conclusion: 'success' });

/**
 * Mock GitHub for an open fork PR whose head commit "approved-sha" passed CI
 * and that an area maintainer approves with /area-maintainer-approved. Tests
 * change the returned state to describe a scenario, then deliver an event
 * with run().
 */
function fixture(t) {
  t.mock.method(fs, 'readFileSync', () =>
    '# Test area (Area Maintainer: @maintainer)\n/test/e2e/ @maintainer\n');
  const headRepository = { id: 1234, owner: { login: 'author' } };
  const pr = {
    number: 6981, state: 'open', user: { login: 'author' },
    head: { sha: 'approved-sha', ref: 'test-branch', repo: headRepository },
  };
  const command = { user: { login: 'maintainer', type: 'User' }, body: '/area-maintainer-approved' };
  const comments = [command];
  const checks = [actionsCheck('e2e')];
  // GitHub returns state "pending" with total_count 0 when a commit has no commit statuses.
  const status = { state: 'pending', total_count: 0, statuses: [] };
  // The ovn-ci run that tested the head. Runs for fork PRs list no pull requests.
  const workflowRun = {
    head_sha: pr.head.sha, pull_requests: [], head_repository: headRepository, head_branch: pr.head.ref,
  };
  const github = {
    rest: {
      repos: { getCombinedStatusForRef: async () => ({ data: status }) },
      pulls: {
        list() {},
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
    // Like GitHub, only return the open PRs of the queried source branch.
    paginate: async (endpoint, query) =>
      query.head === `${pr.head.repo.owner.login}:${pr.head.ref}` ? [pr] : [],
  };
  const run = eventName => areaMerge({
    github,
    // A core.setFailed call fails the test.
    core: { info() {}, setFailed: message => assert.fail(message) },
    context: {
      eventName,
      repo: { owner: 'ovn-kubernetes', repo: 'ovn-kubernetes' },
      payload: eventName === 'issue_comment'
        ? { comment: command, issue: { number: pr.number } }
        : { [eventName]: workflowRun },
    },
  });
  return { pr, comments, checks, status, workflowRun, github, merge: github.rest.pulls.merge, run };
}

/** Report one commit status in the given state, as a non-Actions CI system would. */
function setCommitStatus(f, state) {
  Object.assign(f.status, { state, total_count: 1, statuses: [{ context: 'external-ci', state }] });
}

// What an approval does with the CI results reported for the approved head:
// how many merges it attempts and what the bot replies on the PR.
for (const [outcome, setup, merges, reply] of [
  ['merges when check runs passed and there are no commit statuses',
    () => {}, 1, /^PR merged by area maintainer @maintainer/],
  ['waits when CI has not reported anything yet',
    f => { f.checks.length = 0; }, 0, /^<!-- area-merge-sha:approved-sha -->.*- `\(no CI results reported yet\)`$/s],
  ['waits when only helper jobs and other Apps have reported', f => {
    f.checks.splice(0, 1, actionsCheck('labeler'), { ...actionsCheck('CodeRabbit'), app: { slug: 'coderabbitai' } });
  }, 0, /^<!-- area-merge-sha:approved-sha -->.*- `\(no CI results reported yet\)`$/s],
  ['waits for a pending commit status',
    f => setCommitStatus(f, 'pending'), 0, /^<!-- area-merge-sha:approved-sha -->.*- `external-ci`$/s],
  ['is blocked by a failed commit status',
    f => setCommitStatus(f, 'failure'), 0, /CI checks have failed:\n- `external-ci`\n/],
  ['is blocked by a failed check run',
    f => { f.checks[0].conclusion = 'failure'; }, 0, /CI checks have failed:\n- `e2e`\n/],
]) {
  test(`approval ${outcome}`, async t => {
    const f = fixture(t);
    setup(f);
    await f.run('issue_comment');
    assert.equal(f.merge.mock.callCount(), merges);
    assert.match(f.comments.at(-1).body, reply);
  });
}

test('fork approval waits for CI, then the ovn-ci completion finds the PR and merges it', async t => {
  const f = fixture(t);
  f.checks[0].status = 'in_progress';
  await f.run('issue_comment');
  assert.equal(f.merge.mock.callCount(), 0);
  assert.match(f.comments.at(-1).body, /^<!-- area-merge-sha:approved-sha -->/);

  f.checks[0].status = 'completed';
  await f.run('workflow_run');
  assert.equal(f.merge.mock.callCount(), 1);
  assert.equal(f.merge.mock.calls[0].arguments[0].sha, 'approved-sha');
});

// A completion only finishes an approval that the bot recorded for the PR's current head.
for (const [reason, setup] of [
  ['there is no waiting comment', () => {}],
  ['the waiting comment is for an older head', f => { f.comments.push(waitingComment('old-sha')); }],
  ['a human posted the waiting comment',
    f => { f.comments.push({ ...waitingComment('approved-sha'), user: { login: 'author', type: 'User' } }); }],
  ['the completed run tested an older head',
    f => { f.comments.push(waitingComment('approved-sha')); f.workflowRun.head_sha = 'old-sha'; }],
]) {
  test(`ovn-ci completion does not merge when ${reason}`, async t => {
    const f = fixture(t);
    setup(f);
    const posted = f.comments.length;
    await f.run('workflow_run');
    assert.equal(f.merge.mock.callCount(), 0);
    assert.equal(f.comments.length, posted);
  });
}

test('approval merges when CI completes just before the waiting comment is published', async t => {
  const f = fixture(t);
  f.checks[0].status = 'in_progress';
  const createComment = f.github.rest.issues.createComment;
  t.mock.method(f.github.rest.issues, 'createComment', async params => {
    // The completion is handled first, finds no waiting comment and leaves the PR alone.
    if (params.body.startsWith('<!-- area-merge-sha:')) {
      f.checks[0].status = 'completed';
      await f.run('workflow_run');
      assert.equal(f.merge.mock.callCount(), 0);
    }
    return createComment(params);
  });

  await f.run('issue_comment');
  assert.equal(f.merge.mock.callCount(), 1);
  assert.match(f.comments.at(-1).body, /^PR merged by area maintainer @maintainer/);
});
