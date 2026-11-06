const assert = require('node:assert/strict');
const { test } = require('node:test');
const ciSummary = require('./ci-summary.js');

function fixture() {
  return {
    eventName: 'pull_request',
    needs: {
      changes: { result: 'success', outputs: { run_ci: 'true' } },
      'build-pr': { result: 'success' },
      'test-pr': { result: 'success' },
      e2e: { result: 'success' },
      'ovn-upgrade-e2e': { result: 'success' },
      'e2e-dual-conversion': { result: 'success' },
    },
    skippedOnSchedule: ['ovn-upgrade-e2e', 'e2e-dual-conversion'],
  };
}

test('all dependencies succeeded', () => {
  for (const eventName of ['pull_request', 'merge_group', 'schedule', 'workflow_dispatch', 'push']) {
    assert.deepEqual(ciSummary({ ...fixture(), eventName }), []);
  }
});

test('a failing matrix or unit test blocks the summary', () => {
  const input = fixture();
  input.needs.e2e.result = 'failure';
  input.needs['test-pr'].result = 'failure';
  assert.deepEqual(ciSummary(input), ['test-pr: failure', 'e2e: failure']);
});

test('a failed prerequisite cannot turn skipped tests into a passing summary', () => {
  const input = fixture();
  input.needs['build-pr'].result = 'failure';
  input.needs.e2e.result = 'skipped';
  assert.deepEqual(ciSummary(input), ['build-pr: failure', 'e2e: skipped']);
});

test('cancelled and unexpected skipped dependencies block the summary', () => {
  for (const result of ['cancelled', 'skipped']) {
    const input = fixture();
    input.needs.e2e.result = result;
    assert.deepEqual(ciSummary(input), [`e2e: ${result}`]);
  }
});

test('docs-only PRs can skip expensive jobs but cannot hide failures', () => {
  const input = fixture();
  input.needs.changes.outputs.run_ci = 'false';
  for (const [name, job] of Object.entries(input.needs)) {
    if (name !== 'changes') job.result = 'skipped';
  }
  assert.deepEqual(ciSummary(input), []);
  input.needs.e2e.result = 'failure';
  assert.deepEqual(ciSummary(input), ['e2e: failure']);
});

test('docs exemption does not apply to queue, push, manual or scheduled runs', () => {
  for (const eventName of ['merge_group', 'push', 'workflow_dispatch', 'schedule']) {
    const input = fixture();
    input.eventName = eventName;
    input.needs.changes.outputs.run_ci = 'false';
    assert.deepEqual(ciSummary(input), ['Full CI is required for this event']);
  }
});

test('only explicitly excluded lanes can be skipped on scheduled runs', () => {
  const input = fixture();
  input.eventName = 'schedule';
  input.needs['ovn-upgrade-e2e'].result = 'skipped';
  input.needs['e2e-dual-conversion'].result = 'skipped';
  assert.deepEqual(ciSummary(input), []);
  input.needs.e2e.result = 'skipped';
  assert.deepEqual(ciSummary(input), ['e2e: skipped']);
  input.eventName = 'merge_group';
  assert.equal(ciSummary(input).length, 3);
});

test('changed-file errors and missing outputs block the summary', () => {
  const input = fixture();
  delete input.needs.changes;
  assert.notEqual(ciSummary(input).length, 0);
  input.needs.changes = { result: 'failure', outputs: { run_ci: 'false' } };
  assert.notEqual(ciSummary(input).length, 0);
  input.needs.changes = { result: 'success', outputs: {} };
  assert.notEqual(ciSummary(input).length, 0);
});

test('a newly added dependency is checked without changing the evaluator', () => {
  const input = fixture();
  input.needs['new-e2e-lane'] = { result: 'failure' };
  assert.deepEqual(ciSummary(input), ['new-e2e-lane: failure']);
});

test('a successful failed-job retry clears the summary failure', () => {
  const input = fixture();
  input.needs.e2e.result = 'failure';
  assert.equal(ciSummary(input).length, 1);
  input.needs.e2e.result = 'success';
  assert.deepEqual(ciSummary(input), []);
});
