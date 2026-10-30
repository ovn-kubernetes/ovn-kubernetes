// Evaluate every direct dependency of a workflow's summary job. Matrix jobs
// contribute their aggregate result, so every matrix entry must succeed.
module.exports = function ciSummary({ needs, eventName, skippedOnSchedule = [] }) {
  const changes = needs.changes;
  if (!changes || changes.result !== 'success' ||
      !['true', 'false'].includes(changes.outputs.run_ci)) {
    return ['Changed-file check did not complete successfully'];
  }

  const docsOnly = eventName === 'pull_request' && changes.outputs.run_ci === 'false';
  if (!docsOnly && changes.outputs.run_ci !== 'true') {
    return ['Full CI is required for this event'];
  }

  const failures = [];
  for (const [name, job] of Object.entries(needs)) {
    if (name === 'changes' || job.result === 'success') continue;
    const skipAllowed = docsOnly ||
      (eventName === 'schedule' && skippedOnSchedule.includes(name));
    if (job.result === 'skipped' && skipAllowed) continue;
    failures.push(`${name}: ${job.result}`);
  }
  return failures;
};
