# Notification design language

A shared visual/structural vocabulary for every platform-generated message a human
reads - Slack notifications today, the release PR body, and any future channel. The goal
is one consistent voice, not a Slack-specific reskin: adding a new notification channel
later should mean applying this spec, not inventing a new style.

Direction given directly: blue, not flashy, icons used sparingly - only where they add
real signal, never decoratively.

## Color

One consistent blue accent, `#2563EB`, for normal/success messages. A single muted red,
`#B91C1C` (deliberately not a bright alarm red), reserved for failures only. Not a full
red/yellow/green stoplight - status is legible from the text content already
(`Succeeded`/`Failed` in the header), the color is a secondary, calm signal, not the
primary one.

## Icons

None decoratively. At most one small indicator, `⚠`, prefixed on the header **only**
when the outcome isn't success - so a channel scan surfaces problems without every
routine message being cluttered with a different icon per stage. No per-stage icons
(no 🔨/🧪/🚀/etc.) - stage names are plain text.

## Header

`"<App> · <Stage> <Status>"` - e.g. `"nodejs-demo-app · Build Succeeded"`,
`"⚠ nodejs-demo-app · Release Failed"`. Plain text, no emoji baked into the stage name
itself.

## Field vocabulary

Same labels, same order, everywhere a message has room for them:

| Field | Content | Notes |
|---|---|---|
| App | app name | |
| App Namespace | app namespace | omitted where the audience is external (e.g. the PR body - a GitHub reviewer doesn't care about internal namespace naming) |
| Repo | linked `owner/repo` | derived from `git-url` the same way `open-release-pr.yaml` already derives its owner segment - not reimplemented differently |
| Commit | linked 12-char SHA | 12 chars matches this platform's existing truncation convention from `charts/glidepath-catalog/templates/tasks/build-image.yaml`, not a new one |
| Image | code-formatted image ref | |
| Run | PipelineRun name | Slack only - not meaningful to an external PR reviewer |

Stage-specific additions, only when meaningful (never an empty/placeholder field):
**Environment** on `deploy`, **PR** (linked) on `release` (only present when
`open-release-pr` actually succeeded).

## Footer

A small muted context line on every Slack message: `"platform-cicd · <timestamp>"` - a
consistent signature, not a decoration.

## Per-channel application

- **Slack** (`charts/glidepath-catalog/templates/tasks/notify-slack.yaml`): Block Kit - `header` + `section` with
  `fields` + `context`, wrapped in a single `attachments[0]` entry so the color bar can
  be shown alongside modern blocks (confirmed live against the real webhook before this
  was trusted - Slack's own docs frame `attachments.color` as legacy and don't confirm
  it composes with a `blocks` array, so this was verified empirically, not assumed).
  Failure adds a `*Failed task:* \`<name>\`` line and the existing log-excerpt code
  block, unchanged from Phase 3 item 3's design.
- **Release PR body** (`charts/glidepath-catalog/templates/tasks/open-release-pr.yaml`): the same field
  vocabulary/labels/ordering, in GitHub-flavored markdown. No color - GitHub PR bodies
  can't carry inline color without an embedded badge image, which would itself be the
  kind of flashy this explicitly isn't going for. So the PR body's contribution to "one
  design language" is consistent structure and terminology, not color.

## Why this isn't hardcoded per-message

Every value in every field above was already flowing through the pipeline as an existing
param or task result (`git-url`/`git-revision`/`app-name`/`app-namespace`/`image-ref` are
already Pipeline params on every stage; `open-release-pr`'s `pr-url` result already
existed) - implementing this was entirely about threading already-available data into
`notify-slack.yaml`'s params, not inventing new upstream plumbing.
