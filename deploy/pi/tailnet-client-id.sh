#!/usr/bin/env bash
# Print the client ID of the Pi's Tailscale identity, for the bundle's tailscale-client-id
# (docs/pi-setup.md, "Prepare the bundle's public files").
#
#   deploy/pi/tailnet-client-id.sh >"$prepared/tailscale-client-id"
#
# The infrastructure repository (PI_INFRA_REPO in the Pi's deployment settings) creates that
# identity (PI_TAILNET_IDENTITY) and prints its client ID (PI_TAILNET_CLIENT_ID_OUTPUT) in the log
# of its seed workflow's root job (PI_SEED_WORKFLOW, PI_SEED_JOB). The devbox identity cannot read
# that stack's state, where the ID is an output, but its gh reads those logs.
#
# It reads the seed runs from the newest, every attempt of each run from the newest, and stops at
# the first root job whose log says the identity was created or replaced, so the newest creation
# wins. It never reads past a root job it could not read, because that job may hold a newer ID: a
# root job that ran steps but is still running, or whose log it cannot read, stops it with an error.
# A root job with no steps ran nothing, and is skipped. It reads the runs of the last
# PI_INFRA_LOG_RETENTION_DAYS days, the log retention of that repository: GitHub has deleted older
# logs. It prints the job's link on stderr.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir

fail() {
  printf 'tailnet-client-id: %s\n' "$*" >&2
  exit 1
}

# The office Pi's deployment settings; GHOST_WISPR_PI_ENV names another file, as the tests do.
pi_env=${GHOST_WISPR_PI_ENV:-$script_dir/files/pi.env}
# shellcheck source=files/pi.env
source "$pi_env" || fail "cannot read the Pi deployment settings in $pi_env"
readonly api="repos/$PI_INFRA_REPO/actions"
readonly retention_days=$PI_INFRA_LOG_RETENTION_DAYS
readonly identity=$PI_TAILNET_IDENTITY
readonly output=$PI_TAILNET_CLIENT_ID_OUTPUT

page=1
while :; do
  # GitHub lists the runs newest first. A page with no run inside the retention ends the search.
  runs=$(gh api "$api/workflows/$PI_SEED_WORKFLOW/runs?per_page=100&page=$page" --jq \
    "(now - $retention_days * 86400) as \$cutoff | .workflow_runs[] | select((.created_at | fromdateiso8601) >= \$cutoff) | \"\(.id) \(.run_attempt)\"") \
    || fail "could not list page $page of the seed workflow's runs"
  [[ -n $runs ]] || break
  mapfile -t run_list <<<"$runs"
  for entry in "${run_list[@]}"; do
    read -r run attempts <<<"$entry"
    [[ $run =~ ^[0-9]+$ && $attempts =~ ^[1-9][0-9]*$ ]] || fail "unexpected run from GitHub: $entry"
    for ((attempt = attempts; attempt >= 1; attempt--)); do
      jobs=$(gh api "$api/runs/$run/attempts/$attempt/jobs?per_page=100" \
        --jq ".jobs[] | select(.name == \"$PI_SEED_JOB\") | \"\(.id) \(.status) \(.steps | length)\"") \
        || fail "could not list the jobs of seed run $run, attempt $attempt"
      [[ -n $jobs ]] || continue
      mapfile -t job_list <<<"$jobs"
      for job_entry in "${job_list[@]}"; do
        read -r job status steps <<<"$job_entry"
        [[ $job =~ ^[0-9]+$ && $steps =~ ^[0-9]+$ ]] || fail "unexpected job from GitHub: $job_entry"
        ((steps > 0)) || continue
        link="https://github.com/$PI_INFRA_REPO/actions/runs/$run/job/$job"
        [[ $status == completed ]] || fail "the root job $link is $status: run this again once it has finished"
        log=$(gh api "$api/jobs/$job/logs") || fail "could not read the log of the root job $link"
        grep -Eq "FederatedIdentity $identity (created|replaced)" <<<"$log" || continue
        # A creation prints `+ OUTPUT : "ID"`, a replacement `~ OUTPUT : "OLD" => "ID"`.
        ids=$(sed -n "s/^.* [+~] $output *: \\(\".*\" => \\)\\{0,1\\}\"\\([A-Za-z0-9._:-]*\\)\"\$/\\2/p" <<<"$log" | LC_ALL=C sort -u)
        [[ $ids =~ ^[A-Za-z0-9._:-]+$ ]] \
          || fail "the root job $link made $identity, but its log holds no single $output: ${ids:-none}"
        printf 'tailnet-client-id: %s\n' "$link" >&2
        printf '%s\n' "$ids"
        exit 0
      done
    done
  done
  page=$((page + 1))
done
fail "no seed run of the last $retention_days days made $identity: read the ID from /opt/ghost-wispr/tailscale-client-id on a Pi provisioned before, or ask someone who can read the infrastructure stack's state"
