# Slack notifications

Every stage's pipeline (`build`/`test`/`deploy`/`release`) calls
`charts/glidepath-catalog/templates/tasks/notify-slack.yaml` unconditionally in its `finally` block - one status
message per stage completion, with a failure log excerpt appended when the stage didn't
succeed.

`sast-scan`/`image-scan` (Phase 3 items 8.4/8.5) additionally send their own, separate
shift-left notification the moment each real scan produces a result, rather than waiting
for the stage-level message above. This is controlled by its own
`notifications.slack.scanResults` toggle (default `true`, only takes effect when
`notifications.slack.enabled` is also `true`) - the user asked for this to be optional
independently of the general per-stage notifications, since scan results are more
frequent/verbose and an Application might want one without the other:

```yaml
notifications:
  slack:
    enabled: true
    scanResults: false   # turn off just the sast-scan/image-scan pings, keep the rest
```

## The bug this fixes

The plumbing (`notify-slack.yaml`, reading `notifications.slack` from `cicd.yaml`, a
per-app `slack-webhook-url` Secret) existed since Phase 1, but **never actually
worked**: the script always read `/var/run/secrets/platform/slack-webhook-url`, but the
Task never declared a `volumes:`/`volumeMounts:` for it at all. Even an Application that fully
enabled `notifications.slack` and created the Secret exactly as the old header comment
described would still get nothing - the script's own graceful "no secret mounted" skip
path silently absorbed the missing file, so this looked like an unconfigured-app no-op
rather than a broken feature. Confirmed live before fixing: no Application has ever had this
secret, and `kubectl get externalsecret,clustersecretstore -A` returns nothing anywhere -
External Secrets Operator is installed but has zero configured backends, so the
architecture doc's original "populated by ESO at onboarding" plan for this secret was
never actually built. Fixed at the time with a plain `secret` volume (`optional: true`,
so Applications without it still start cleanly - not a new failure mode, matches the
script's existing skip behavior), not a full ESO SecretStore pipeline for one demo
webhook.

**That tradeoff was revisited once a second real consumer needed the identical
mechanism** - see [app-secrets.md](app-secrets.md). The volume mount is unchanged in
shape (still `optional: true`, still one file per Secret data key), but now sources
from `app-secrets` (ESO-synced from the Application's own backend store) instead of a
hand-created, per-Application Secret.

## Onboarding an Application (per app)

1. **Enable it in `cicd.yaml`**, declaring the secret alongside it:
   ```yaml
   secrets:
     - name: slack-webhook-url
   notifications:
     slack:
       enabled: true
       channel: "#your-channel"
   ```
2. **Populate the Application's own backend secret store** with a `slack-webhook-url`
   key holding a real Slack incoming-webhook URL - see
   [app-secrets.md](app-secrets.md) for where that store is assumed to live.

   Nothing else to apply - `notify-slack.yaml`'s volume mount is already wired into
   every pipeline via the existing, unconditional `notify` finally task.

## Message format

- Always: `[<app-namespace>] <stage-name> <status>: <pipeline-run-name>` - one line, every
  stage, every outcome.
- On any non-success outcome (`Failed`, `Cancelled`, `PipelineRunTimeout`, etc. - not
  narrowed to just `Failed`, since a short excerpt of whatever the last-running step
  logged is useful context regardless of the exact failure mode): a second block naming
  the failed TaskRun and a tail of its pod logs (last 30 lines, capped to ~1500
  characters as a readability limit, not Slack's actual `text` size limit), wrapped in a
  Slack code block.

No new RBAC was needed for the log-fetching step - `pipeline-runner`'s existing Role
(`charts/glidepath-app/templates/identity/pipeline-runner.yaml`) already grants `get`/`list`/
`watch` on `taskruns` (tekton.dev) and `pods`/`pods/log` (core), confirmed by re-reading
that file rather than assumed.

## Verification

- Mount fix, in isolation: create a real `slack-webhook-url` Secret, enable
  `notifications.slack` for a real Application, run a real pipeline, confirm an actual message
  lands in the real Slack channel - this is the part that had never once worked, so it
  needs to be seen working, not assumed fixed by re-reading the diff.
- Failure-log test: a synthetic PipelineRun with a deliberately bad param (same surgical-
  break technique used elsewhere in Phase 3), confirming the Slack message includes a
  real, readable excerpt from the actually-failed step.
- No-secret regression check: an Application without the Secret configured still completes a
  normal pipeline run with no error - the `optional: true` mount plus the script's
  existing skip path, not a new failure mode.
