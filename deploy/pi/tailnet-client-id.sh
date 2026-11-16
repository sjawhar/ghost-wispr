#!/usr/bin/env bash
# Print the client ID of the Tailscale identity ghost-wispr-x509, for the bundle's
# tailscale-client-id (docs/pi-setup.md, "Prepare the bundle's public files").
#
#   deploy/pi/tailnet-client-id.sh >"$prepared/tailscale-client-id"
#
# agent-infra's root program creates that identity and prints its client ID in the log of the seed
# workflow's root job. The devbox identity cannot read root's Pulumi state, where the ID is a stack
# output, but its gh reads those logs.
#
# It reads the seed runs from the newest, every attempt of each run from the newest, and stops at
# the first root job whose log says ghost-wispr-x509 was created or replaced, so the newest creation
# wins. It never reads past a root job it could not read, because that job may hold a newer ID: a
# root job that ran steps but is still running, or whose log it cannot read, stops it with an error.
# A root job with no steps ran nothing, and is skipped. It reads the runs of the last 90 days, the
# log retention agent-infra sets (root/retention.json): GitHub has deleted older logs. It prints
# the job's link on stderr.
set -euo pipefail

readonly api=repos/theorem-labs/agent-infra/actions
readonly retention_days=90

fail() {
  printf 'tailnet-client-id: %s\n' "$*" >&2
  exit 1
}

page=1
while :; do
  # GitHub lists the runs newest first. A page with no run of the last 90 days ends the search.
  runs=$(gh api "$api/workflows/seed.yaml/runs?per_page=100&page=$page" --jq \
    "(now - $retention_days * 86400) as \$cutoff | .workflow_runs[] | select((.created_at | fromdateiso8601) >= \$cutoff) | \"\(.id) \(.run_attempt)\"") \
    || fail "could not list page $page of the seed workflow's runs"
  [[ -n $runs ]] || break
  mapfile -t run_list <<<"$runs"
  for entry in "${run_list[@]}"; do
    read -r run attempts <<<"$entry"
    [[ $run =~ ^[0-9]+$ && $attempts =~ ^[1-9][0-9]*$ ]] || fail "unexpected run from GitHub: $entry"
    for ((attempt = attempts; attempt >= 1; attempt--)); do
      jobs=$(gh api "$api/runs/$run/attempts/$attempt/jobs?per_page=100" \
        --jq '.jobs[] | select(.name == "root") | "\(.id) \(.status) \(.steps | length)"') \
        || fail "could not list the jobs of seed run $run, attempt $attempt"
      [[ -n $jobs ]] || continue
      mapfile -t job_list <<<"$jobs"
      for job_entry in "${job_list[@]}"; do
        read -r job status steps <<<"$job_entry"
        [[ $job =~ ^[0-9]+$ && $steps =~ ^[0-9]+$ ]] || fail "unexpected job from GitHub: $job_entry"
        ((steps > 0)) || continue
        link="https://github.com/theorem-labs/agent-infra/actions/runs/$run/job/$job"
        [[ $status == completed ]] || fail "the root job $link is $status: run this again once it has finished"
        log=$(gh api "$api/jobs/$job/logs") || fail "could not read the log of the root job $link"
        grep -Eq 'FederatedIdentity ghost-wispr-x509 (created|replaced)' <<<"$log" || continue
        # A creation prints `+ ghostWisprTailnetClientId : "ID"`, a replacement `~ ... : "OLD" => "ID"`.
        ids=$(sed -n 's/^.* [+~] ghostWisprTailnetClientId *: \(".*" => \)\{0,1\}"\([A-Za-z0-9._:-]*\)"$/\2/p' <<<"$log" | LC_ALL=C sort -u)
        [[ $ids =~ ^[A-Za-z0-9._:-]+$ ]] \
          || fail "the root job $link made ghost-wispr-x509, but its log holds no single ghostWisprTailnetClientId: ${ids:-none}"
        printf 'tailnet-client-id: %s\n' "$link" >&2
        printf '%s\n' "$ids"
        exit 0
      done
    done
  done
  page=$((page + 1))
done
fail "no seed run of the last $retention_days days made ghost-wispr-x509: read the ID from /opt/ghost-wispr/tailscale-client-id on a Pi provisioned before, or ask someone who can read root's Pulumi state"
