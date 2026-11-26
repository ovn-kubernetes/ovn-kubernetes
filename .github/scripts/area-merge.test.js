const assert = require('node:assert/strict');
const fs = require('node:fs');
const { test } = require('node:test');
const areaMerge = require('./area-merge');

const botLogin = process.env.BOT_LOGIN || 'ovn-kubernetes-merge-bot[bot]';
const waitingComment = sha => ({ user: { login: botLogin, type: 'Bot' }, body: `<!-- area-merge-sha:${sha} -->` });

/** A successful check run of a GitHub Actions job. */
const actionsCheck = name =>
  ({ name, app: { slug: 'github-actions' }, status: 'completed', conclusion: 'success' });

/** A review in which the area maintainer approves the fixture PR's head and gives the command. */
const commandReview = changes => ({
  user: { login: 'maintainer', type: 'User' }, state: 'APPROVED', commit_id: 'approved-sha',
  body: '/area-maintainer-approved', submitted_at: '2026-01-02T00:00:00Z', ...changes,
});

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
  const reviews = [];
  const checks = [actionsCheck('e2e')];
  // GitHub returns state "pending" with total_count 0 when a commit has no commit statuses.
  const status = { state: 'pending', total_count: 0, statuses: [] };
  // The ovn-ci run that tested the head. Runs for fork PRs list no pull requests.
  const workflowRun = {
    head_sha: pr.head.sha, pull_requests: [], head_repository: headRepository, head_branch: pr.head.ref,
  };
  // The "Pull Request Review" run of a review that the area maintainer submitted.
  // For a fork PR it reports the upstream repository and lists no pull requests.
  const reviewRun = {
    event: 'pull_request_review', display_title: `Review PR ${pr.number}`, actor: { login: 'maintainer' },
    head_sha: pr.head.sha, pull_requests: [], head_repository: { id: 1, owner: { login: 'ovn-kubernetes' } },
    head_branch: pr.head.ref,
  };
  const github = {
    rest: {
      repos: { getCombinedStatusForRef: async () => ({ data: status }) },
      pulls: {
        list() {},
        listReviews() {},
        get: async ({ pull_number }) => {
          assert.equal(pull_number, pr.number);
          return { data: pr };
        },
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
    paginate: async (endpoint, query) => {
      if (endpoint === github.rest.pulls.listReviews) return reviews;
      return query.head === `${pr.head.repo.owner.login}:${pr.head.ref}` ? [pr] : [];
    },
  };
  const run = (eventName, source = workflowRun) => areaMerge({
    github,
    // A core.setFailed call fails the test.
    core: { info() {}, setFailed: message => assert.fail(message) },
    context: {
      eventName,
      repo: { owner: 'ovn-kubernetes', repo: 'ovn-kubernetes' },
      payload: eventName === 'issue_comment'
        ? { comment: command, issue: { number: pr.number } }
        : { [eventName]: source },
    },
  });
  return {
    pr, comments, reviews, checks, status, workflowRun, reviewRun, github, merge: github.rest.pulls.merge, run,
  };
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
  ['the command is in a review that got no waiting comment',
    f => { f.comments.length = 0; f.reviews.push(commandReview()); }],
  ['the review with the command was dismissed while waiting for CI', f => {
    f.comments.splice(0, 1, waitingComment('approved-sha'));
    f.reviews.push(commandReview({ state: 'DISMISSED' }));
  }],
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

// What the bot does when a review is submitted. Except in the first case, the
// PR also has the fixture's older comment command, which must not be honored
// in place of the review: nothing ties it to the PR's current head.
for (const [outcome, setup, merges, reply] of [
  ['merges when it approves the head and starts with the command',
    f => { f.comments.length = 0; f.reviews.push(commandReview()); }, 1, /^PR merged by area maintainer @maintainer/],
  ['is rejected when the reviewer is not an area maintainer for the changed files', f => {
    f.reviews.push(commandReview({ user: { login: 'other', type: 'User' } }));
    f.reviewRun.actor.login = 'other';
  }, 0, /^@other cannot merge this PR/],
  ['is ignored when it approves an older commit',
    f => { f.reviews.push(commandReview({ commit_id: 'old-sha' })); }, 0],
  ['is ignored when it does not approve the PR',
    f => { f.reviews.push(commandReview({ state: 'COMMENTED' })); }, 0],
  ['is ignored when it was dismissed',
    f => { f.reviews.push(commandReview({ state: 'DISMISSED' })); }, 0],
  ['is ignored when it does not start with the command',
    f => { f.reviews.push(commandReview({ body: 'LGTM' })); }, 0],
  ['is ignored when the PR author submitted it', f => {
    f.reviews.push(commandReview({ user: { login: 'author', type: 'User' } }));
    f.reviewRun.actor.login = 'author';
  }, 0],
  ['is ignored when a bot submitted it',
    f => { f.reviews.push(commandReview({ user: { login: 'maintainer', type: 'Bot' } })); }, 0],
  ['is ignored when the command was in an earlier review of the same reviewer',
    f => { f.reviews.push(commandReview(), commandReview({ state: 'COMMENTED', body: 'A nit.' })); }, 0],
  ['is ignored when the command was in the review of another reviewer', f => {
    f.reviews.push(commandReview(), commandReview({ user: { login: 'other', type: 'User' }, body: 'LGTM' }));
    f.reviewRun.actor.login = 'other';
  }, 0],
  ['is ignored when its run does not name the PR',
    f => { f.reviews.push(commandReview()); f.reviewRun.display_title = 'Review PR'; }, 0],
]) {
  test(`submitted review ${outcome}`, async t => {
    const f = fixture(t);
    setup(f);
    const posted = f.comments.length;
    await f.run('workflow_run', f.reviewRun);
    assert.equal(f.merge.mock.callCount(), merges);
    if (reply) assert.match(f.comments.at(-1).body, reply);
    else assert.equal(f.comments.length, posted);
  });
}

test('approving review with the command waits for CI, then the ovn-ci completion merges the PR', async t => {
  const f = fixture(t);
  f.comments.length = 0;
  f.reviews.push(commandReview());
  f.checks[0].status = 'in_progress';
  await f.run('workflow_run', f.reviewRun);
  assert.equal(f.merge.mock.callCount(), 0);
  assert.match(f.comments.at(-1).body, /^<!-- area-merge-sha:approved-sha -->\nArea maintainer @maintainer /);

  f.checks[0].status = 'completed';
  await f.run('workflow_run');
  assert.equal(f.merge.mock.callCount(), 1);
  assert.equal(f.merge.mock.calls[0].arguments[0].sha, 'approved-sha');
});

// A completion honors the most recent command, whether it is a comment or a
// review. Here someone who is not an area maintainer gave the comment command.
for (const [latest, commented, merges, reply] of [
  ['review', '2026-01-01T00:00:00Z', 1, /^PR merged by area maintainer @maintainer/],
  ['comment', '2026-01-03T00:00:00Z', 0, /^@other cannot merge this PR/],
]) {
  test(`ovn-ci completion honors the ${latest} when it is the most recent command`, async t => {
    const f = fixture(t);
    Object.assign(f.comments[0], { user: { login: 'other', type: 'User' }, created_at: commented });
    f.comments.push(waitingComment('approved-sha'));
    f.reviews.push(commandReview());
    await f.run('workflow_run');
    assert.equal(f.merge.mock.callCount(), merges);
    assert.match(f.comments.at(-1).body, reply);
  });
}
