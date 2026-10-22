# Changelog

## v6.2.0 (2026-10-21)

### Behavior changes

- **A check that fails from the start runs its `UnlockOnlyAfterHealthy` actions.** The
  action runner started in `critical`, so a service that booted broken looked "already
  failed" and its `FailureAction` (and `WarningAction`) never ran. A new runner now starts
  with no result (`initializing`); a runner restored after a restart keeps its saved
  status, so a check still failing in the same incident does not run them again.
- **Actions with neither `Timeout` nor `UnlockAfterDuration` now run.** They never
  executed before: the cooldown doubled as the command's deadline, so a cooldown of 0
  killed the command as it started (only a `run:error` tag said so). This hit every
  `UnlockOnlyAfterHealthy` action without a cooldown, including the README's
  `SuccessAction` example. They now run, bounded by `DefaultActionTimeout` (60s). An
  action that sat silent in production will start firing after the upgrade: that is the
  fix working.
- **New `Action.Timeout`** bounds an action's command, independent of its cooldown. Unset,
  the deadline is still the cooldown when one is set, so existing configurations behave
  as before. A long cooldown without `Timeout` still lets a hung command run that long
  (a wedged remediation script then holds a goroutine and its reserved event sequence
  number): set `Timeout` explicitly on every action.
- **The overall status is the worst check's** (passing < warning < initializing <
  timeout < critical), whatever order the checks are merged in. Before, a `timeout` or
  `initializing` status could be replaced by any check merged after it, so a service with
  a timed-out check answered 200 or 503 depending on scheduling; it now always answers 503.
- **Timeouts are never softened by `SkipOnErr`.** `SkipOnErr` only turns an error a check
  returns into a warning; a check that times out is `timeout` and runs its
  `TimeoutAction`. A `SkipOnErr` check that times out makes the service answer 503 (it
  could answer 429 before, by merge order).
- **A check interval below 1s is raised to 1s** (`MinimumInterval`); it was replaced by
  10s, so a 200ms check ran fifty times slower than asked. An unset interval still gets
  the 10s default (`DefaultInterval`).
