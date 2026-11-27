---
title: "deploy/pi shell on the CI runner: uutils date and systemd 259"
category: general
tags:
  - deploy
  - shell
  - ci
  - systemd
  - coreutils
date: 2026-11-26
status: active
module: deploy/pi
symptoms:
  - "update test FAILED (unit-limits): systemd --test did not read the update unit"
  - "a time from date +%s%6N compares the wrong way"
  - "a deploy/pi test passes on the dev box and fails in CI"
---

# deploy/pi shell on the CI runner: uutils date and systemd 259

## Context

The `deploy/pi` scripts and their tests run in three places: the Raspberry Pi (Raspberry Pi OS, GNU coreutils), a developer's machine, and the GitHub runner. In November 2026 the runner's image was `ubuntu-26.04` (its CI log names the image). That image differs from Ubuntu 24.04 in two ways that broke tests which had passed on a dev box. Both were found while building the whole-deploy-set updater (sjawhar/ghost-wispr#201, pending).

## Guidance

1. **Do not use `date +%N` or `%6N` for sub-second times.** Ubuntu 26.04's `date` is uutils coreutils. Its `%6N` prints a fraction under 0.1 s without its leading zeros: 0.050 s prints `500000`. Two times on either side of a tenth of a second then compare the wrong way. Read the fraction yourself instead:
   - a file's time: `LC_ALL=C find FILE -maxdepth 0 -printf '%T@'` and split at the dot (`file_time_us` in `deploy/pi/update.sh`);
   - a unit's start time: `systemctl show -p ExecMainStartTimestamp --timestamp=us+utc --value`, and parse its `.NNNNNN` part (`timestamp_us` in the same file).
   Use `date` for the whole seconds alone.
2. **Read systemd's unit dump with either arrow.** `systemd --test --system --unit=…` heads each unit's section with a tab, an arrow and `Unit NAME:`. systemd 255 (Ubuntu 24.04, and a dev box) prints `->`; systemd 259 (Ubuntu 26.04) prints `→`, and `->` again when `LANG` is unset. Match both: `/^\t(->|→) Unit NAME:$/` (the `unit-limits` case in `deploy/pi/update_test.sh`).
3. **A test that needs systemd's own parser fails loudly when systemd is absent.** The runner image has systemd and `systemd-analyze`; a bare `ubuntu` container does not. Make the test name what it needs and fail, so a missing tool never reads as a pass.
4. **A test written to catch a time bug must sit on the boundary the bug crosses.** A first test used 0.049, 0.050 and 0.051 s. With uutils `%6N` those still compare the right way, so the test passed against the broken code. The test that catches it puts the two times on either side of 0.1 s (0.099500 and 0.100500).

## When this applies

Any shell in `deploy/pi/` that compares times, or reads `systemd` or `systemctl` output, and any test of it. Reproduce the runner before you push: run the test in an `ubuntu:26.04` container with systemd installed, as a non-root user, both with `LANG=C.UTF-8` and with `LANG` unset.
