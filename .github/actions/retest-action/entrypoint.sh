#!/bin/sh
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0


set -ex

##############################
# Prerequisites check
##############################

if ! jq -e '.issue.pull_request' "${GITHUB_EVENT_PATH}"; then
    echo "Not a PR... Exiting."
    exit 0
fi

COMMENT_BODY=$(jq -r '.comment.body' "${GITHUB_EVENT_PATH}")
if [ "${COMMENT_BODY}" != "/retest" ] &&
  [ "${COMMENT_BODY}" != "/retest-failed" ] &&
  [ "${COMMENT_BODY}" != "/cancel" ] &&
  [ "${COMMENT_BODY}" != "/help" ]; then
    echo "Unknown action. Nothing to do... Exiting."
    exit 0
fi

##############################
# functions section
##############################

send_reaction() {
  REACTION_SYMBOL="$1"
  REACTION_URL="$(jq -r '.comment.url' "${GITHUB_EVENT_PATH}")/reactions"
  curl --silent \
       --request POST \
       --url "${REACTION_URL}" \
       --header "authorization: Bearer ${GITHUB_TOKEN}" \
       --header "accept: application/vnd.github.squirrel-girl-preview+json" \
       --header "content-type: application/json" \
       --data '{ "content" : "'"${REACTION_SYMBOL}"'" }'
}

send_comment() {
  COMMENT="$1"
  COMMENTS_URL=$(jq -r '.issue.comments_url' "${GITHUB_EVENT_PATH}")
  curl --silent \
       --request POST \
       --url "${COMMENTS_URL}" \
       --header "authorization: Bearer ${GITHUB_TOKEN}" \
       --header "accept: application/vnd.github.squirrel-girl-preview+json" \
       --header "content-type: application/json" \
       --data '{ "body" : "'"${COMMENT}"'" }'
}

##############################
# logic section
##############################

ACTION="${COMMENT_BODY}"

if [ "$ACTION" = "/help" ]; then
	send_comment "Supported operations are /retest, /retest-failed, /cancel"
	exit 0
fi

PR_URL=$(jq -r '.issue.pull_request.url' "${GITHUB_EVENT_PATH}")

curl --silent \
     --request GET \
     --url "${PR_URL}" \
     --header "authorization: Bearer ${GITHUB_TOKEN}" \
     --header "content-type: application/json" \
    > pr.json

ACTOR=$(jq -r '.user.login' pr.json)
BRANCH=$(jq -r '.head.ref' pr.json)

# GET /actions/runs is served from an index that is not strongly consistent:
# it intermittently answers 200 with an empty workflow_runs list for a query
# that has matches (see https://github.com/orgs/community/discussions/24626).
# Retry a few times before concluding there is nothing to trigger, and report
# HTTP errors instead of treating them as "no runs".
RUNS_URL="https://api.github.com/repos/${GITHUB_REPOSITORY}/actions/runs?event=pull_request&actor=${ACTOR}&branch=${BRANCH}"
LIST_ATTEMPTS=5
attempt=1
while :; do
  RESPONSE_CODE=$(curl --silent \
      --write-out '%{http_code}' \
      --output runs-response.json \
      --request GET \
      --url "${RUNS_URL}" \
      --header "authorization: Bearer ${GITHUB_TOKEN}" \
      --header "content-type: application/json")
  if ! echo "${RESPONSE_CODE}" | grep -E -q '^2'; then
    send_reaction "confused"
    send_comment "Failed to list workflow runs for ${BRANCH} (HTTP ${RESPONSE_CODE}): $(jq -r '.message // "no error message"' runs-response.json | tr -d '"')"
    exit 0
  fi
  TOTAL=$(jq -r '.total_count // 0' runs-response.json)
  if [ "${TOTAL}" -gt 0 ] || [ "${attempt}" -ge "${LIST_ATTEMPTS}" ]; then
    break
  fi
  echo "Workflow run listing for ${BRANCH} came back empty (attempt ${attempt}/${LIST_ATTEMPTS}), retrying..."
  attempt=$((attempt + 1))
  sleep 5
done

jq '.workflow_runs | group_by(.name) | map(max_by(.run_number))' runs-response.json \
  > workflow_runs.json

[ -f "workflow_runs.json" ] && cat workflow_runs.json

if [ "$ACTION" = "/retest" ]; then
  jq -r 'map(select(.status|contains("completed"))) | .[] | .rerun_url' workflow_runs.json \
    > url.data
elif [ "$ACTION" = "/retest-failed" ]; then
  # New feature, rerun failed jobs:
  # https://docs.github.com/en/rest/reference/actions#re-run-failed-jobs-from-a-workflow-run
  jq -r 'map(select(.status|contains("completed"))) | map(select(.conclusion|contains("failure"))) | .[] | .rerun_url + "-failed-jobs"' workflow_runs.json \
    > url.data
elif [ "$ACTION" = "/cancel" ]; then
  jq -r 'map(select(.status | test ("queued|in_progress|pending" )) | .[] | .cancel_url' workflow_runs.json \
    > url.data
else
  echo "Something went wrong, unsupported action"
  exit 0
fi

REACTION_SYMBOL="rocket"
for url in $(cat url.data); do
  # Execute the action.
  # Store the response code in a variable.
  # Store the answer in file .action-response.json.
  RESPONSE_CODE=$(curl --silent \
      --write-out '%{http_code}' \
      --output .action-response.json \
      --request POST \
      --url "${url}" \
      --header "authorization: Bearer ${GITHUB_TOKEN}" \
      --header "content-type: application/json")

  if ! echo "${RESPONSE_CODE}" | grep -E -q '^2'; then
    REACTION_SYMBOL="confused"
    RESPONSE_MESSAGE=$(jq -r '.message' .action-response.json)
    send_comment "Oops, something went wrong when triggering workflow run\n${url}\n~~~\n${RESPONSE_MESSAGE}\n~~~\n"
    break
  fi
  touch triggered.data
  echo "$url" | sed -e 's|/api.github.com/repos/|/github.com/|' -e 's|/[^/]*$||' >> triggered.data
  rm .action-response.json
done

if [ -f "triggered.data" ]; then
  RESPONSE_MESSAGE="The following workflows runs were succesfully triggered:\n$(cat triggered.data)"
  send_comment "${RESPONSE_MESSAGE}"
else
  REACTION_SYMBOL="confused"
  RESPONSE_MESSAGE="There was an error or no workflows were found in an appropriate state to be triggered"
  send_comment "${RESPONSE_MESSAGE}"
fi

send_reaction "${REACTION_SYMBOL}"
