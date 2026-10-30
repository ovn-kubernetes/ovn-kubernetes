const assert = require('node:assert/strict');
const fs = require('node:fs');
const { test } = require('node:test');
const areaMerge = require('./area-merge.js');

function fixture(t, queueEnabled = false) {
  const readFileSync = fs.readFileSync;
  t.mock.method(fs, 'readFileSync', (path, ...args) => path === 'CODEOWNERS'
    ? '# Area Maintainer: @alice\n/area/ @alice\n'
    : readFileSync(path, ...args));
  const pr = {
    state: 'open', user: { login: 'author' },
    head: { sha: 'reviewed-sha' }, base: { ref: 'master' },
  };
  const command = {
    id: 1, body: '/area-maintainer-approved',
    user: { login: 'alice', type: 'User' },
    created_at: '2026-10-27T12:00:00Z',
  };
  const state = {
    pr, comments: [command], files: [{ filename: 'area/code.go' }],
    checks: [{ name: 'Test-PR', status: 'completed', conclusion: 'success' }],
    status: { state: 'success', statuses: [] },
    repository: {
      mergeQueue: queueEnabled ? { id: 'queue-id' } : null,
      pullRequest: {
        id: 'pr-id', headRefOid: pr.head.sha, baseRefName: pr.base.ref,
        mergeQueueEntry: null, timelineItems: { nodes: [] },
      },
    },
    merges: [], enqueues: [], errors: [], logs: [], queries: [],
  };
  const context = {
    repo: { owner: 'ovn-kubernetes', repo: 'ovn-kubernetes' },
    eventName: 'issue_comment',
    payload: { issue: { number: 1 }, comment: command },
  };
  const github = {
    graphql: async (query, variables) => {
      if (query.includes('mutation')) {
        state.enqueues.push(variables);
        if (state.enqueueError) throw new Error(state.enqueueError);
        state.repository.pullRequest.mergeQueueEntry = { id: 'entry-id' };
        state.repository.pullRequest.timelineItems.nodes = [{ createdAt: '2026-10-27T12:01:00Z' }];
        return { enqueuePullRequest: { mergeQueueEntry: { id: 'entry-id' } } };
      }
      state.queries.push(variables);
      if (state.queryError) throw new Error(state.queryError);
      return { repository: state.repository };
    },
    rest: {
      pulls: {
        get: async () => ({ data: state.pr }),
        listFiles: async () => ({ data: state.files }),
        merge: async args => { state.merges.push(args); },
      },
      issues: {
        listComments: async () => ({ data: state.comments }),
        createComment: async ({ body }) => {
          state.comments.push({ body, user: { login: 'ovn-kubernetes-merge-bot[bot]' } });
        },
      },
      checks: {
        listForRef: async () => ({ data: { check_runs: state.checks, total_count: state.checks.length } }),
      },
      repos: { getCombinedStatusForRef: async () => ({ data: state.status }) },
    },
  };
  const core = {
    info: message => state.logs.push(message),
    setFailed: message => state.errors.push(message),
  };
  return {
    state, context, command,
    run: () => areaMerge({ github, context, core }),
    completeChecks: () => {
      context.eventName = 'check_suite';
      context.payload = { check_suite: { head_branch: 'feature', pull_requests: [{ number: 1 }] } };
      state.checks[0].status = 'completed';
    },
  };
}

test('keeps SHA-guarded direct merges when the target has no queue', async t => {
  const { state, run } = fixture(t);
  await run();
  assert.equal(state.merges.length, 1);
  assert.equal(state.merges[0].sha, 'reviewed-sha');
  assert.equal(state.merges[0].merge_method, 'merge');
  assert.equal(state.enqueues.length, 0);
  assert.equal(state.queries[0].branch, 'master');
});

test('enqueues the authorized SHA instead of merging when the target has a queue', async t => {
  const { state, run } = fixture(t, true);
  await run();
  assert.deepEqual(state.enqueues, [{ id: 'pr-id', sha: 'reviewed-sha' }]);
  assert.equal(state.merges.length, 0);
  assert.match(state.comments.at(-1).body, /PR queued/);
});

for (const queueEnabled of [false, true]) {
  test(`preserves author and file-scope restrictions (queue=${queueEnabled})`, async t => {
    const { state, command, run } = fixture(t, queueEnabled);
    command.user.login = 'author';
    await run();
    command.user.login = 'alice';
    state.files = [{ filename: 'outside/code.go' }];
    await run();
    assert.equal(state.merges.length + state.enqueues.length, 0);
    assert.equal(state.queries.length, 0);
  });

  test(`does not act after an unresolved queue lookup (queue=${queueEnabled})`, async t => {
    const { state, run } = fixture(t, queueEnabled);
    state.queryError = 'permission denied';
    await run();
    assert.equal(state.merges.length + state.enqueues.length, 0);
    assert.match(state.errors[0], /permission denied/);
  });

  for (const field of ['headRefOid', 'baseRefName']) {
    test(`rejects a changed ${field} (queue=${queueEnabled})`, async t => {
      const { state, run } = fixture(t, queueEnabled);
      state.repository.pullRequest[field] = 'changed';
      await run();
      assert.equal(state.merges.length + state.enqueues.length, 0);
      assert.match(state.errors[0], /changed/);
    });
  }

  test(`enforces existing failed jobs and commit statuses (queue=${queueEnabled})`, async t => {
    const { state, run } = fixture(t, queueEnabled);
    state.checks[0].conclusion = 'failure';
    await run();
    state.checks[0].conclusion = 'success';
    state.status = { state: 'failure', statuses: [{ state: 'failure', context: 'DCO' }] };
    await run();
    assert.equal(state.merges.length + state.enqueues.length, 0);
  });

  test(`resumes after initial CI succeeds (queue=${queueEnabled})`, async t => {
    const { state, run, completeChecks } = fixture(t, queueEnabled);
    state.checks[0].status = 'in_progress';
    await run();
    assert.equal(state.merges.length + state.enqueues.length, 0);
    assert.match(state.comments.at(-1).body, queueEnabled ? /will enqueue/ : /will merge/);
    completeChecks();
    await run();
    assert.equal(state.merges.length + state.enqueues.length, 1);
  });

  test(`new commits invalidate the waiting authorization (queue=${queueEnabled})`, async t => {
    const { state, run, completeChecks } = fixture(t, queueEnabled);
    state.checks[0].status = 'in_progress';
    await run();
    state.pr.head.sha = 'new-sha';
    state.repository.pullRequest.headRefOid = 'new-sha';
    completeChecks();
    await run();
    assert.equal(state.merges.length + state.enqueues.length, 0);
  });
}

for (const name of ['CI Summary', 'DPU CI Summary']) {
  for (const status of ['in_progress', 'completed']) {
    test(`ignores staged ${name} only with queue disabled (${status})`, async t => {
      const { state, run } = fixture(t);
      state.checks.push({ name, status, conclusion: 'failure' });
      await run();
      assert.equal(state.merges.length, 1);
      state.repository.mergeQueue = { id: 'queue-id' };
      await run();
      assert.equal(state.enqueues.length, 0);
      assert.equal(state.merges.length, 1);
    });
  }
}

test('an enqueue rejection never falls back to merging', async t => {
  const { state, run } = fixture(t, true);
  state.enqueueError = 'required approvals missing';
  await run();
  assert.equal(state.enqueues.length, 1);
  assert.equal(state.merges.length, 0);
  assert.match(state.errors[0], /required approvals missing/);
});

test('does not enqueue an already queued PR again', async t => {
  const { state, run } = fixture(t, true);
  await run();
  await run();
  assert.equal(state.enqueues.length, 1);
});

test('removal requires a new command, not a later check-suite completion', async t => {
  const { state, context, command, run, completeChecks } = fixture(t, true);
  state.checks[0].status = 'in_progress';
  await run();
  completeChecks();
  await run();
  state.repository.pullRequest.mergeQueueEntry = null;
  // Applies to both failed queue validation and deliberate removal by a maintainer.
  state.repository.pullRequest.timelineItems.nodes = [{ createdAt: '2026-10-27T12:02:00Z' }];
  await run();
  assert.equal(state.enqueues.length, 1);
  context.eventName = 'issue_comment';
  context.payload = { issue: { number: 1 }, comment: { ...command, id: 2, created_at: '2026-10-27T12:03:00Z' } };
  await run();
  assert.equal(state.enqueues.length, 2);
});

test('ignores queue check-suite events', async t => {
  const { state, context, run, completeChecks } = fixture(t, true);
  completeChecks();
  context.payload.check_suite.head_branch = 'gh-readonly-queue/master/pr-1-sha';
  await run();
  assert.equal(state.queries.length, 0);
  assert.equal(state.merges.length + state.enqueues.length, 0);
});
