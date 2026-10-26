# Proposal: Backups — Daily Schedules

## Problem

A backup job only runs when someone presses **Run** in the UI or calls
`POST /api/backup/configs/{id}/run`. Nothing in tierd starts a job on its own.
Several comments (`PurgeBackupsUnderPath`, `destroyTierPool`) already talk about
"the backup schedule", but no scheduler exists.

An offsite backup that depends on someone remembering to press a button is not
a backup. The Google Drive method (backup-02) needs this most: Google caps
uploads at roughly 750 GB per day, so a first upload of a few terabytes only
finishes if the job runs again every day until it is done.

## Decisions (locked)

- **Per-job, all methods.** The schedule is a property of a backup config, not
  of a method. `cp`, `rsync`, and the new `rclone` method all get it.
- **Daily at a set time, on chosen weekdays.** No cron expressions. One time of
  day plus a weekday set covers the real cases, and is easy to show and validate
  in the UI.
- **Catch up once, never stack.** If tierd was down when a job was due, it runs
  once when tierd comes back (if the missed slot was in the last 24 hours). Missed
  days are never replayed one after another.
- **Never overlap.** If the job already has a run in progress when it comes due,
  the slot is skipped, and the next slot is the next chance.

## Specification

### Storage

Migration `00028_backup_schedule.sql` adds four columns to `backup_configs`:

| Column | Type | Meaning |
|---|---|---|
| `schedule_enabled` | INTEGER NOT NULL DEFAULT 0 | 0 = manual only (every existing job keeps today's behaviour) |
| `schedule_time` | TEXT NOT NULL DEFAULT '' | `HH:MM`, 24-hour, in the NAS's local time zone |
| `schedule_days` | INTEGER NOT NULL DEFAULT 127 | weekday bitmask, bit 0 = Monday … bit 6 = Sunday |
| `last_scheduled_at` | TEXT NOT NULL DEFAULT '' | RFC 3339 UTC time of the last slot the scheduler claimed |

`db.BackupConfig` gains `ScheduleEnabled`, `ScheduleTime`, `ScheduleDays`,
`LastScheduledAt`, and a computed, read-only `NextRunAt` filled in by the API.

### Scheduler

A new `internal/backup/schedule` component, owned by `BackupHandler` and started
with the daemon:

- It ticks every 30 seconds. On each tick it lists the configs that have
  `schedule_enabled = 1`.
- For each one it computes the most recent due slot at or before now. A slot is
  due when it is later than `last_scheduled_at` and no more than 24 hours old.
- To claim a slot, it does a conditional `UPDATE … SET last_scheduled_at = ? WHERE
  id = ? AND last_scheduled_at = ?` **before** starting the run. A restart, or a
  second tick, can then never start the same slot twice.
- It starts the run through the same `startRun` path that a manual run uses,
  so drain handling, resume and progress all behave the same.
- If the config already has a `running` run, it claims the slot and skips it,
  and logs one line at info level. Skipping is not an error, and it doesn't
  create a failed run.
- It does nothing while the handler is draining (`isDraining()`).

The NAS's local time zone is the reference, so "02:00" means 02:00 local time,
across DST changes too. A slot that doesn't exist on a spring-forward day runs
at the first valid minute after it. A slot that occurs twice on a fall-back day
runs once.

### API

`POST/PUT /api/backup/configs` accept `schedule_enabled`, `schedule_time` and
`schedule_days`. Validation rules:

- `schedule_time` must match `^([01]\d|2[0-3]):[0-5]\d$` when scheduling is
  enabled.
- `schedule_days` must be between 1 and 127 when scheduling is enabled.

`GET` responses include `next_run_at` (empty when scheduling is off).

### UI

The backup config form gains a **Schedule** section: an on/off switch, a time
input, and seven weekday toggles that all default to on. The job list shows
**Next run** next to the last run's status. All new strings go through i18n
like the rest of the Backups page (see i18n-02o).

## Testing

- Unit tests for slot computation with a fixed clock: normal days, a weekday
  mask, the 24-hour catch-up window, both DST transitions, and a slot claimed
  twice.
- Handler test: a scheduled slot with a run already in progress is claimed and
  skipped, and no new run row is created.
- Restart test: claim a slot, rebuild the scheduler, and confirm the same slot
  does not start again.

## Out of scope

- Cron expressions, intervals shorter than a day, and schedules that run more
  than once a day.
- Notifications on failure. Run history already records failures, and alerting
  belongs in a separate proposal.
