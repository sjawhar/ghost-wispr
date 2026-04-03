# Ghost Wispr Real-World Benchmark

## Key results

- **Peter exact-notes query**
  - without skill: **correct** in 4 calls (`20260327203708`)
  - with skill v1: wrong session in 13 calls (`20260328073102`)
  - with skill v2: wrong session in 18 calls (`20260328044039`)
  - with final skill: **correct** in 7 calls (`20260327203708`)

- **Recent priorities query**
  - without skill: mostly correct in 10 calls
  - with skill: mostly correct in 6 calls

- **Sami day-audit query**
  - without skill: wrong date drift in 12 calls (`20260328023411`)
  - with skill v1: miss / false negative in 16 calls
  - with final skill: **correct** in 8 calls (`20260326024958-merged`)

## What the benchmark taught us

1. Broad context queries benefit from the skill's semantic-first guidance.
2. Exact-notes queries need **concept-first / phrase-first** retrieval, not speaker-first filtering.
3. Day-audit queries need date-bounded session listing plus transcript fallback when `segments` is empty.
4. `session.refined_transcript` and `session.canonical_transcript` are essential fallback sources.

## Final verdict

The updated skill is materially better than the initial draft:
- it still helps on broad context search,
- it now handles the Peter exact-notes case correctly,
- and it fixes the March 26 Sami day-audit failure mode.
