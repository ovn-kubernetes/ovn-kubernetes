#!/bin/sh

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

curl --silent \
     --request GET \
     --url "https://api.github.com/repos/${GITHUB_REPOSITORY}/actions/runs?event=pull_request&actor=${ACTOR}&branch=${BRANCH}" \
     --header "authorization: Bearer ${GITHUB_TOKEN}" \
     --header "content-type: application/json" |\
  jq '.workflow_runs | group_by(.name) | map(max_by(.run_number))' \
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
  RESPONSE_MESSAGE="It looks like the previous CI run failed, please report failed tests: this is the only way to get them fixed! See https://ovn-kubernetes.io/ci/ci#ci-fails-what-do-i-do for more details on what to do. The following workflows runs were successfully triggered:\n$(cat triggered.data)"
  send_comment "${RESPONSE_MESSAGE}"
else
  REACTION_SYMBOL="confused"
  RESPONSE_MESSAGE="There was an error or no workflows were found in an appropriate state to be triggered"
  send_comment "${RESPONSE_MESSAGE}"
fi

send_reaction "${REACTION_SYMBOL}"
