#!/usr/bin/env bash
# Every GitHub Action the workflows use is pinned by full commit SHA, with its tag as a comment.
# The release workflow builds the binary the Pi's nightly update installs, so a moved tag must not
# change what runs there.
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root

shopt -s nullglob
workflows=("$repo_root"/.github/workflows/*.yml "$repo_root"/.github/workflows/*.yaml)
if ((${#workflows[@]} == 0)); then
  printf 'UNKNOWN: no workflow files under %s/.github/workflows\n' "$repo_root" >&2
  exit 1
fi

checked=0
failed=0
for workflow in "${workflows[@]}"; do
  line_number=0
  while IFS= read -r line; do
    line_number=$((line_number + 1))
    [[ $line =~ ^[[:space:]]*-?[[:space:]]*uses:[[:space:]]*(.*)$ ]] || continue
    checked=$((checked + 1))
    reference=${BASH_REMATCH[1]}
    if [[ ! $reference =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40}[[:space:]]+#[[:space:]]+v[0-9]+(\.[0-9]+)*$ ]]; then
      printf '%s:%d: not pinned by full commit SHA with its tag as a comment: %s\n' \
        "${workflow#"$repo_root"/}" "$line_number" "$reference" >&2
      failed=$((failed + 1))
    fi
  done <"$workflow"
done

if ((checked == 0)); then
  printf 'UNKNOWN: no uses: lines found in %d workflow files\n' "${#workflows[@]}" >&2
  exit 1
fi
if ((failed > 0)); then
  printf '%d of %d action references are not pinned\n' "$failed" "$checked" >&2
  exit 1
fi
printf 'workflow pin test passed: %d action references pinned\n' "$checked"
