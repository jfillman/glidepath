# Notifications

Two independent targets, toggled separately per app in `cicd.yaml`: [Slack](#slack-notifications) and [Backstage](#backstage-notifications). Both fire from the same call sites - an app can run either, both, or neither.

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

# Backstage notifications

Sibling to the Slack path above, sending the same general build/test/deploy/release
pass-fail message (`charts/glidepath-catalog/templates/tasks/notify-backstage.yaml`,
called from the same unconditional `finally` block as `notify-slack.yaml`, right next to
it) into Backstage's own Notifications plugin (`@backstage/plugin-notifications-backend`,
already installed and wired in the `backstage` repo - `packages/backend/src/index.ts`)
instead of a Slack channel. Toggled independently via `notifications.backstage.enabled`
in `cicd.yaml` - an app can run Slack, Backstage, both, or neither. Viewable in Tower's
Notifications tab and via the stock Backstage sidebar bell icon
(`NotificationsSidebarItem`, already present).

Unlike Slack's per-Application webhook, there is exactly one Backstage instance
platform-wide (not one per cluster) - every CI/CD Pipeline runs on the dev cluster
(see `devClusterName`), but Backstage itself runs on a different cluster entirely. So
the target URL and credential are **cluster-level chart values, not per-Application
`cicd.yaml`/secrets entries**, and reaching Backstage at all is a real cross-cluster
call, not an in-cluster one:

1. **Configure `glidepath-catalog`'s `backstageBaseUrl`** (a cluster-config value, set
   via the `platform-cicd-catalog` Application's `valuesObject` in `gitops-cluster-dev`
   - not the chart's own tracked default) to Backstage's real externally-reachable
   Gateway hostname, e.g. `http://backstage.prod.kiac.local:7007`. **Not** an in-cluster
   Service DNS name (`backstage.<ns>.svc.cluster.local`) - that only resolves on
   Backstage's own cluster, not the dev cluster the pipeline actually runs on.
2. **Set `glidepath-app`'s `backstageHostAliasIP`** to that same hostname's current real
   IP. `backstage.prod.kiac.local` isn't real DNS anywhere a pod's CoreDNS can see - it
   only resolves on a developer's own laptop, via `refresh-kiac-hosts.sh` writing
   `/etc/hosts` after a kiac VM restart. `notify-backstage`'s Task pod needs the same
   hostname baked into its own `hostAliases` (`flow-triggers.yaml`/
   `release-outcome-trigger.yaml`'s `taskRunSpecs`) to resolve it at build time. kiac's
   `container` runtime hands out a fresh IP on every VM boot (no static-address option),
   so - like Fulcio material and the Infisical IP elsewhere in this platform - this is a
   manually-maintained value: bump it (`container list`, the target cluster's own
   control-plane address) whenever that cluster restarts, in step with whatever value
   step 1 above uses.
3. **Mint a Backstage static service token** scoped to the notifications plugin only
   (`backend.auth.externalAccess`, `type: static`, `accessRestrictions: [{plugin:
   notifications}]` - see Backstage's own [service-to-service auth
   docs](https://backstage.io/docs/auth/service-to-service-auth/)). Populate it under the
   `backstage-notify-token` key in the same backend secret store `glidepath-control-plane`'s
   `secretStore` already reads from - `templates/secretstore/glidepath-backstage-notify-
   cluster-external-secret.yaml` disseminates it into every managed namespace
   automatically (same mechanism as `registry-credentials`, one shared credential
   platform-wide, not one per Application).
4. **Enable it per app** in `cicd.yaml`:
   ```yaml
   notifications:
     backstage:
       enabled: true
   ```

Nothing else to apply - `notify-backstage.yaml`'s volume mount is already wired into
every pipeline via the existing, unconditional `notify-backstage` finally task.

## Message format

- Same event coverage and stage vocabulary as Slack's (`Build`/`Test`/`Deploy`/`Release`
  pass/fail, plus `release-outcome`), but mapped onto Backstage's own notification
  fields rather than Slack's block-kit format: `title` (`<app> · <stage> <status>`),
  `description` (app/repo/commit/image/environment/PR lines, plus a failure-log excerpt
  on non-success, same as Slack's), `link` (the same Tekton Dashboard deep-link, with
  the same "dead link unless port-forwarded locally" caveat), `severity` (`normal` on
  success, `high` otherwise - Backstage renders this as its own icon/color, so no `⚠`
  text is added the way Slack's header gets one), and `topic` (the stage name, letting a
  Backstage user filter per-topic in their own per-user notification settings - a native
  capability Slack's channel model doesn't have).
- Always broadcast (`recipients: {type: "broadcast"}`) - visible to every Backstage user,
  not targeted at a specific owner/team. No per-app recipient targeting today; a future
  enhancement could resolve the app's catalog-info.yaml `spec.owner` instead, if the
  broadcast-only model turns out too noisy at scale.

## Descoped from this pass

`sast-scan.yaml`/`image-scan.yaml`'s separate shift-left scan-result Slack messages
(`notifications.slack.scanResults`) were **not** given a Backstage equivalent - those
Tasks inline their own Slack-specific curl/formatting logic rather than calling
`notify-slack.yaml`, so mirroring them means duplicating `notify-backstage.yaml`'s logic
into both Tasks rather than reusing it. Left as a follow-up if per-scan Backstage
notifications turn out to be wanted; the general per-stage notification above already
covers the common case.

## Verification

- Static-token path, in isolation: create the `glidepath-backstage-notify` Secret and
  configure `backstageBaseUrl` for a real cluster, enable `notifications.backstage` for a
  real Application, run a real pipeline, confirm a real notification lands in Backstage
  (sidebar bell unread count increments, Tower's Notifications tab shows it).
- No-config regression check: an Application with `notifications.backstage.enabled` but
  no cluster-level `backstageBaseUrl`/secret configured still completes a normal pipeline
  run with no error - same `optional: true` mount plus graceful skip as the Slack path.
- Both-targets check: an Application with both `notifications.slack.enabled` and
  `notifications.backstage.enabled` true gets one message in each system per stage
  completion, independently of each other.
