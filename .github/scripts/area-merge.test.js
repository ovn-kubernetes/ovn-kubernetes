const assert = require('node:assert/strict');
const fs = require('node:fs');
const { test } = require('node:test');
const areaMerge = require('./area-merge');

/**
 * Build an approved PR fixture with mutable CI state and mocked GitHub APIs.
 * The test context restores the CODEOWNERS read stub after each test.
 */
function fixture(t, eventName = 'workflow_run') {
  t.mock.method(fs, 'readFileSync', () =>
    '# Test area (Area Maintainer: @maintainer)\n/test/e2e/ @maintainer\n');
  const botLogin = process.env.BOT_LOGIN || 'ovn-kubernetes-merge-bot[bot]';
  const headRepository = { id: 1234, owner: { login: 'author' } };
  const pr = {
    number: 6981, state: 'open', user: { login: 'author' },
    head: { sha: 'approved-sha', ref: 'test-branch', repo: headRepository },
  };
  const command = { user: { login: 'maintainer', type: 'User' }, body: '/area-maintainer-approved' };
  const comments = [command, {
    user: { login: botLogin, type: 'Bot' },
    body: '<!-- area-merge-sha:approved-sha -->',
  }];
  const checks = [{ name: 'e2e', status: 'completed', conclusion: 'success' }];
  const status = { state: 'success', statuses: [] };
  const source = {
    id: 123, event: 'pull_request', head_sha: pr.head.sha, pull_requests: [],
    head_repository: headRepository, head_branch: pr.head.ref,
  };
  const associatedPRs = [pr];
  const branchPRs = [pr];
  const context = {
    eventName,
    repo: { owner: 'ovn-kubernetes', repo: 'ovn-kubernetes' },
    sha: 'default-branch-sha',
    payload: { [eventName]: source, comment: command, issue: { number: pr.number } },
  };
  const github = {
    rest: {
      repos: {
        listPullRequestsAssociatedWithCommit: t.mock.fn(),
        getCombinedStatusForRef: async () => ({ data: status }),
      },
      pulls: {
        list: t.mock.fn(),
        get: t.mock.fn(async () => ({ data: pr })),
        listFiles: t.mock.fn(async () => ({ data: [{ filename: 'test/e2e/kubevirt.go' }] })),
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
        listForRef: t.mock.fn(async () => ({ data: { check_runs: checks, total_count: checks.length } })),
      },
    },
    paginate: t.mock.fn(async endpoint => {
      if (endpoint === github.rest.repos.listPullRequestsAssociatedWithCommit) return associatedPRs;
      assert.equal(endpoint, github.rest.pulls.list);
      return branchPRs;
    }),
  };
  const core = { info() {}, setFailed: message => assert.fail(message) };
  return {
    pr, source, command, comments, checks, status, context, github, core, associatedPRs, branchPRs,
    merge: github.rest.pulls.merge,
    run: (eventContext = context) => areaMerge({ github, context: eventContext, core }),
  };
}

test('fork approval waits for CI, then workflow completion merges the approved SHA', async t => {
  const f = fixture(t, 'issue_comment');
  f.comments.pop();
  f.checks[0].status = 'in_progress';
  await f.run();
  assert.equal(f.merge.mock.callCount(), 0);
  assert.match(f.comments[1].body, /<!-- area-merge-sha:approved-sha -->/);
  assert.equal(f.comments.length, 2);
  assert.equal(f.github.rest.checks.listForRef.mock.callCount(), 2);

  f.checks[0].status = 'completed';
  f.context.eventName = 'workflow_run';
  f.context.payload.workflow_run = f.source;
  await f.run();
  assert.equal(f.merge.mock.callCount(), 1);
  assert.equal(f.merge.mock.calls[0].arguments[0].sha, 'approved-sha');
  const [endpoint, params] = f.github.paginate.mock.calls[0].arguments;
  assert.equal(endpoint, f.github.rest.repos.listPullRequestsAssociatedWithCommit);
  assert.equal(params.commit_sha, 'approved-sha');
  assert.equal(params.per_page, 100);
});

for (const eventName of ['workflow_run', 'check_suite']) {
  test(`approval merges when ${eventName} finishes before the waiting marker is published`, async t => {
    const f = fixture(t, 'issue_comment');
    f.comments.pop();
    f.checks[0].status = 'in_progress';
    const createComment = f.github.rest.issues.createComment;
    t.mock.method(f.github.rest.issues, 'createComment', async params => {
      if (params.body.startsWith('<!-- area-merge-sha:')) {
        f.checks[0].status = 'completed';
        await f.run({
          ...f.context,
          eventName,
          payload: { ...f.context.payload, [eventName]: f.source },
        });
        assert.equal(f.merge.mock.callCount(), 0);
        assert.equal(f.github.rest.checks.listForRef.mock.callCount(), 1);
      }
      return createComment(params);
    });

    await f.run();
    assert.equal(f.merge.mock.callCount(), 1);
    assert.equal(f.merge.mock.calls[0].arguments[0].sha, 'approved-sha');
    assert.match(f.comments.at(-1).body, /PR merged by area maintainer @maintainer/);
  });

  test(`approval tolerates ${eventName} merging after the waiting marker is published`, async t => {
    const f = fixture(t, 'issue_comment');
    f.comments.pop();
    f.checks[0].status = 'in_progress';
    f.merge.mock.mockImplementation(async ({ sha }) => {
      assert.equal(sha, 'approved-sha');
      if (f.pr.merged) throw new Error('Pull Request is not mergeable');
      f.pr.state = 'closed';
      f.pr.merged = true;
      return { data: { merged: true } };
    });
    const createComment = f.github.rest.issues.createComment;
    t.mock.method(f.github.rest.issues, 'createComment', async params => {
      const result = await createComment(params);
      if (params.body.startsWith('<!-- area-merge-sha:')) {
        f.checks[0].status = 'completed';
        await f.run({
          ...f.context,
          eventName,
          payload: { ...f.context.payload, [eventName]: f.source },
        });
        assert.equal(f.merge.mock.callCount(), 1);
      }
      return result;
    });

    await f.run();
    assert.equal(f.merge.mock.callCount(), 2);
    assert.equal(f.github.rest.pulls.get.mock.callCount(), 3);
    assert.equal(f.comments.length, 3);
    assert.match(f.comments.at(-1).body, /PR merged by area maintainer @maintainer/);
    assert.ok(f.comments.every(c => !c.body.includes('Failed to merge')));
  });

  test(`${eventName} resolves fork PRs by SHA`, async t => {
    const f = fixture(t, eventName);
    await f.run();
    assert.equal(f.github.paginate.mock.callCount(), 1);
    assert.equal(f.merge.mock.callCount(), 1);
  });

  test(`${eventName} uses payload PRs when present`, async t => {
    const f = fixture(t, eventName);
    f.source.pull_requests = [{ number: f.pr.number }];
    await f.run();
    assert.equal(f.github.paginate.mock.callCount(), 0);
    assert.equal(f.merge.mock.callCount(), 1);
  });

  test(`${eventName} requires a waiting marker for the current head`, async t => {
    const f = fixture(t, eventName);
    f.comments[1].body = '<!-- area-merge-sha:old-sha -->';
    await f.run();
    assert.equal(f.merge.mock.callCount(), 0);
    assert.equal(f.github.rest.checks.listForRef.mock.callCount(), 0);
  });
}

for (const failure of ['check run', 'commit status']) {
  test(`approval reports a ${failure} failure found after publishing the waiting marker`, async t => {
    const f = fixture(t, 'issue_comment');
    f.comments.pop();
    f.checks[0].status = 'in_progress';
    f.github.rest.checks.listForRef.mock.mockImplementationOnce(async () => {
      assert.match(f.comments.at(-1).body, /<!-- area-merge-sha:approved-sha -->/);
      f.checks[0].status = 'completed';
      if (failure === 'check run') {
        f.checks[0].conclusion = 'failure';
      } else {
        f.status.state = 'failure';
        f.status.statuses = [{ context: 'external-ci', state: 'failure' }];
      }
      return { data: { check_runs: f.checks, total_count: f.checks.length } };
    }, 1);

    await f.run();
    assert.equal(f.merge.mock.callCount(), 0);
    assert.match(f.comments.at(-1).body, /CI checks have failed/);
    assert.match(f.comments.at(-1).body, /will merge automatically when all checks pass/);
    assert.match(f.comments.at(-1).body, failure === 'check run' ? /- `e2e`/ : /- `external-ci`/);
  });
}

test('approval keeps the CI recheck and merge pinned when the head changes after the waiting marker', async t => {
  const f = fixture(t, 'issue_comment');
  f.comments.pop();
  f.checks[0].status = 'in_progress';
  const createComment = f.github.rest.issues.createComment;
  t.mock.method(f.github.rest.issues, 'createComment', async params => {
    const result = await createComment(params);
    if (params.body.startsWith('<!-- area-merge-sha:')) {
      f.pr.head.sha = 'unreviewed-sha';
      f.checks[0].status = 'completed';
    }
    return result;
  });
  const getCombinedStatusForRef = f.github.rest.repos.getCombinedStatusForRef;
  t.mock.method(f.github.rest.repos, 'getCombinedStatusForRef', async params => {
    assert.equal(params.ref, 'approved-sha');
    return getCombinedStatusForRef(params);
  });

  await f.run();
  assert.deepEqual(f.github.rest.checks.listForRef.mock.calls.map(c => c.arguments[0].ref),
    ['approved-sha', 'approved-sha']);
  assert.equal(f.merge.mock.callCount(), 1);
  assert.equal(f.merge.mock.calls[0].arguments[0].sha, 'approved-sha');
});

test('workflow completion without a waiting marker cannot initiate a merge', async t => {
  const f = fixture(t);
  f.comments.pop();
  await f.run();
  assert.equal(f.merge.mock.callCount(), 0);
});

test('workflow completion resolves a fork PR by branch when commit lookup is empty', async t => {
  const f = fixture(t);
  f.associatedPRs.length = 0;
  await f.run();
  assert.equal(f.github.paginate.mock.callCount(), 2);
  const [endpoint, params] = f.github.paginate.mock.calls[1].arguments;
  assert.equal(endpoint, f.github.rest.pulls.list);
  assert.deepEqual(params, {
    owner: 'ovn-kubernetes', repo: 'ovn-kubernetes',
    state: 'open', head: 'author:test-branch', per_page: 100,
  });
  assert.equal(f.merge.mock.callCount(), 1);
  assert.equal(f.merge.mock.calls[0].arguments[0].sha, 'approved-sha');
});

for (const [field, value] of [['repo', { id: 5678 }], ['ref', 'other-branch'], ['sha', 'old-sha']]) {
  test(`branch discovery rejects a candidate with a different head ${field}`, async t => {
    const f = fixture(t);
    f.associatedPRs.length = 0;
    f.branchPRs[0] = { ...f.pr, head: { ...f.pr.head, [field]: value } };
    await f.run();
    assert.equal(f.github.rest.pulls.get.mock.callCount(), 0);
    assert.equal(f.merge.mock.callCount(), 0);
  });
}

for (const field of ['head_repository', 'head_branch']) {
  test(`branch discovery skips an event without ${field}`, async t => {
    const f = fixture(t);
    f.associatedPRs.length = 0;
    delete f.source[field];
    await f.run();
    assert.equal(f.github.paginate.mock.callCount(), 1);
    assert.equal(f.merge.mock.callCount(), 0);
  });
}

test('branch discovery still requires a bot waiting marker', async t => {
  const f = fixture(t);
  f.associatedPRs.length = 0;
  f.comments.pop();
  await f.run();
  assert.equal(f.github.paginate.mock.callCount(), 2);
  assert.equal(f.merge.mock.callCount(), 0);
});

test('a human cannot forge the bot waiting marker', async t => {
  const f = fixture(t);
  f.comments[1].user = f.command.user;
  await f.run();
  assert.equal(f.merge.mock.callCount(), 0);
});

test('an older workflow completion cannot retry a newer PR head', async t => {
  const f = fixture(t);
  f.source.head_sha = 'old-sha';
  await f.run();
  assert.equal(f.merge.mock.callCount(), 0);
  assert.equal(f.github.rest.checks.listForRef.mock.callCount(), 0);
});

test('SHA discovery ignores closed PRs', async t => {
  const f = fixture(t);
  f.pr.state = 'closed';
  f.branchPRs.length = 0;
  await f.run();
  assert.equal(f.github.rest.pulls.get.mock.callCount(), 0);
  assert.equal(f.merge.mock.callCount(), 0);
});

test('a queued retry skips a PR merged by the preceding run', async t => {
  const f = fixture(t);
  f.source.pull_requests = [{ number: f.pr.number }];
  f.merge.mock.mockImplementation(async () => {
    f.pr.state = 'closed';
    return { data: { merged: true } };
  });
  await f.run();
  await f.run();
  assert.equal(f.github.rest.pulls.get.mock.callCount(), 2);
  assert.equal(f.merge.mock.callCount(), 1);
  assert.equal(f.github.rest.issues.createComment.mock.callCount(), 1);
});

for (const [scenario, state, merged, sha] of [
  ['still open', 'open', false, 'approved-sha'],
  ['closed without merging', 'closed', false, 'approved-sha'],
  ['merged at a different SHA', 'closed', true, 'other-sha'],
]) {
  test(`merge errors are reported when the PR is ${scenario}`, async t => {
    const f = fixture(t);
    const setFailed = t.mock.method(f.core, 'setFailed', () => {});
    f.merge.mock.mockImplementation(async () => { throw new Error('merge rejected'); });
    f.github.rest.pulls.get.mock.mockImplementationOnce(async () => ({
      data: { ...f.pr, state, merged, head: { ...f.pr.head, sha } },
    }), 1);

    await f.run();
    assert.equal(f.github.rest.pulls.get.mock.callCount(), 2);
    assert.deepEqual(setFailed.mock.calls.map(c => c.arguments),
      [[`Failed to merge PR #${f.pr.number}: merge rejected`]]);
    assert.equal(f.github.rest.issues.createComment.mock.callCount(), 1);
    assert.match(f.comments.at(-1).body, /Failed to merge this PR: `merge rejected`/);
    assert.match(f.comments.at(-1).body, /A repo maintainer may need to merge manually/);
  });
}

test('a failed PR refresh preserves the original merge error', async t => {
  const f = fixture(t);
  const setFailed = t.mock.method(f.core, 'setFailed', () => {});
  f.merge.mock.mockImplementation(async () => { throw new Error('merge rejected'); });
  f.github.rest.pulls.get.mock.mockImplementationOnce(async () => {
    throw new Error('PR lookup unavailable');
  }, 1);

  await f.run();
  assert.equal(f.github.rest.pulls.get.mock.callCount(), 2);
  assert.deepEqual(setFailed.mock.calls.map(c => c.arguments),
    [[`Failed to merge PR #${f.pr.number}: merge rejected`]]);
  assert.match(f.comments.at(-1).body, /Failed to merge this PR: `merge rejected`/);
});

test('workflow completions from pushes are ignored', async t => {
  const f = fixture(t);
  f.source.event = 'push';
  await f.run();
  assert.equal(f.github.paginate.mock.callCount(), 0);
  assert.equal(f.merge.mock.callCount(), 0);
});

test('workflow completion rechecks CI rather than trusting its conclusion', async t => {
  const f = fixture(t);
  f.source.conclusion = 'success';
  f.checks.push({ name: 'performance', status: 'in_progress' });
  await f.run();
  assert.equal(f.merge.mock.callCount(), 0);
  assert.equal(f.github.rest.issues.createComment.mock.callCount(), 0);
});

test('a failing check blocks the retry', async t => {
  const f = fixture(t);
  f.checks[0].conclusion = 'failure';
  await f.run();
  assert.equal(f.merge.mock.callCount(), 0);
  assert.match(f.comments.at(-1).body, /CI checks have failed/);
});

for (const state of ['failure', 'error']) {
  test(`a commit status with state ${state} blocks the retry`, async t => {
    const f = fixture(t);
    // GitHub aggregates both failure and error contexts into state "failure".
    f.status.state = 'failure';
    f.status.statuses = [{ context: 'external-ci', state }];
    await f.run();
    assert.equal(f.merge.mock.callCount(), 0);
    assert.match(f.comments.at(-1).body, /external-ci/);
  });
}

for (const eventName of ['workflow_run', 'check_suite']) {
  test(`${eventName} reports the same failures only once regardless of order`, async t => {
    const f = fixture(t, eventName);
    f.checks[0].conclusion = 'failure';
    f.checks.push({ name: 'unit', status: 'completed', conclusion: 'failure' });
    await f.run();
    assert.match(f.comments.at(-1).body, /- `e2e`\n- `unit`/);
    assert.match(f.comments.at(-1).body, /will merge automatically when all checks pass/);
    f.checks.reverse();
    await f.run();
    assert.equal(f.github.rest.issues.createComment.mock.callCount(), 1);
    assert.equal(f.merge.mock.callCount(), 0);

    for (const check of f.checks) check.conclusion = 'success';
    await f.run();
    assert.equal(f.merge.mock.callCount(), 1);
  });
}

test('duplicate failure names across checks and statuses do not repeat notifications', async t => {
  const f = fixture(t);
  f.checks[0].conclusion = 'failure';
  f.status.state = 'failure';
  f.status.statuses = [{ context: 'e2e', state: 'failure' }];
  await f.run();
  assert.equal(f.comments.at(-1).body.match(/- `e2e`/g).length, 1);

  f.status.state = 'success';
  f.status.statuses[0].state = 'success';
  await f.run();
  assert.equal(f.github.rest.issues.createComment.mock.callCount(), 1);
  assert.equal(f.merge.mock.callCount(), 0);
});

test('different failure sets containing commas receive separate notifications', async t => {
  const f = fixture(t);
  f.checks[0].name = 'e2e,ipv6';
  f.checks[0].conclusion = 'failure';
  f.checks.push({ name: 'unit', status: 'completed', conclusion: 'failure' });
  await f.run();

  f.checks[0].name = 'e2e';
  f.checks[1].name = 'ipv6,unit';
  await f.run();
  assert.equal(f.github.rest.issues.createComment.mock.callCount(), 2);
  assert.match(f.comments.at(-1).body, /- `e2e`\n- `ipv6,unit`/);
  assert.equal(f.merge.mock.callCount(), 0);
});

test('failure names cannot terminate the hidden failure marker', async t => {
  const f = fixture(t);
  f.checks[0].name = 'e2e --> "ipv6"';
  f.checks[0].conclusion = 'failure';
  await f.run();
  const marker = f.comments.at(-1).body.split('\n')[0];
  assert.match(marker, /^<!-- area-merge-failed-sha:approved-sha:/);
  assert.equal(marker.indexOf('-->'), marker.length - 3);
  assert.match(f.comments.at(-1).body, /- `e2e --> "ipv6"`/);

  await f.run();
  assert.equal(f.github.rest.issues.createComment.mock.callCount(), 1);
  assert.equal(f.merge.mock.callCount(), 0);
});

test('retries report changed failures and failures on a newly approved SHA', async t => {
  const f = fixture(t);
  f.checks[0].conclusion = 'failure';
  await f.run();
  f.status.state = 'failure';
  f.status.statuses = [{ context: 'external-ci', state: 'failure' }];
  await f.run();
  assert.equal(f.github.rest.issues.createComment.mock.callCount(), 2);
  assert.match(f.comments.at(-1).body, /external-ci/);

  f.pr.head.sha = f.source.head_sha = 'new-approved-sha';
  f.comments[1].body = '<!-- area-merge-sha:new-approved-sha -->';
  await f.run();
  assert.equal(f.github.rest.issues.createComment.mock.callCount(), 3);
  assert.equal(f.merge.mock.callCount(), 0);
});

test('a human failure marker cannot suppress a retry notification', async t => {
  const f = fixture(t);
  f.checks[0].conclusion = 'failure';
  await f.run();
  f.comments.at(-1).user = f.command.user;
  await f.run();
  assert.equal(f.github.rest.issues.createComment.mock.callCount(), 2);
  assert.equal(f.merge.mock.callCount(), 0);
});

for (const waiting of [false, true]) {
  test(`explicit commands always report failures with waiting=${waiting}`, async t => {
    const f = fixture(t, 'issue_comment');
    if (!waiting) f.comments.pop();
    f.checks[0].conclusion = 'failure';
    await f.run();
    await f.run();
    assert.equal(f.github.rest.issues.createComment.mock.callCount(), 2);
    assert.equal(f.merge.mock.callCount(), 0);
    if (waiting) {
      assert.match(f.comments.at(-1).body, /will merge automatically when all checks pass/);
    } else {
      assert.match(f.comments.at(-1).body, /Please fix the failures and run `\/area-maintainer-approved` again/);
      assert.doesNotMatch(f.comments.at(-1).body, /will merge automatically/);
    }
  });
}

test('workflow completion rechecks area authorization', async t => {
  const f = fixture(t);
  f.command.user.login = 'other-reviewer';
  await f.run();
  assert.equal(f.merge.mock.callCount(), 0);
  assert.match(f.comments.at(-1).body, /not listed as an Area Maintainer/);
});

test('workflow completion cannot use the PR author as approver', async t => {
  const f = fixture(t);
  f.pr.user.login = 'maintainer';
  await f.run();
  assert.equal(f.merge.mock.callCount(), 0);
});
