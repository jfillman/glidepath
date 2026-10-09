# Quickstart

Get from zero to a running pipeline. This assumes your app repo has already been
through platform-side onboarding once - see [install-guide.md](install-guide.md) if
you're setting up a brand new app for the first time. This guide is about the part
you'll actually repeat: writing and iterating on `cicd.yaml`.

## 1. Write the smallest `cicd.yaml` that builds your app

At your repo root:

```yaml
apiVersion: platform/v1
kind: PipelineConfig

build:
  agent: nodejs-20        # nodejs-18/20/22, openjdk-17/21, python-3.11, go-1.22
  script: ./build.sh      # your build script - runs inside the agent image

pipelines:
  ci:
    trigger:
      source: git
      event: push
      branch: main
    steps:
      - stage: build
```

That's a complete, valid pipeline: push to `main`, and the platform builds your app,
runs unit tests (`./test.sh` by default), packages a container image, and pushes it to
`ghcr.io/<your-org>/<your-app>`. Nothing deploys yet - that's step 3 below.

See [examples/01-minimal-build-only.yaml](examples/01-minimal-build-only.yaml) for this
exact file with every field explained inline.

## 2. Validate before you push

```bash
yajsv -s schemas/cicd.schema.json <(yq -o=json . cicd.yaml)
```

This is the *exact* check the `preflight` Task's validate step runs at the start of every
real pipeline. Catching a typo here costs you two seconds; catching it after a push costs you
a burned pipeline run and a wait.

## 3. Push, and watch it happen

```bash
git add cicd.yaml
git commit -m "Add platform-cicd pipeline"
git push origin main
```

Within a few seconds you should see a new commit status / check appear on GitHub against
that commit - that's Pipelines-as-Code reporting the pipeline's progress in real time.
If nothing appears after a minute or two, see "If nothing happens" below.

## 4. Add a test stage

```yaml
test:
  name: integration   # the TestWorkflow name every test step inherits by default

pipelines:
  ci:
    trigger: { source: git, event: push, branch: main }
    steps:
      - stage: build
      - stage: test
        env: dev
```

`stage: test` is **event-chained** - it isn't triggered by a second git push, it's
triggered automatically the moment `build` finishes and publishes its image. You don't
configure that wiring; declaring the step is enough. `env: dev` and a resolvable test
name are both required on every test step - see
[cicd-yaml-reference.md](cicd-yaml-reference.md#the-test-block) for why.

## 5. Add a deploy stage

```yaml
deploy:
  environments:              # this is the default - shown for clarity
    - { name: dev, tier: ground }

pipelines:
  ci:
    trigger: { source: git, event: push, branch: main }
    steps:
      - stage: build
      - stage: test
        env: dev
      - stage: deploy
        env: dev
```

Same idea: `deploy` fires automatically once `test` reports success. Your app now has a
real, standing dev deployment that updates on every push to `main`. `env: dev` must
be listed in `deploy.environments` - that's what actually
provisions the RBAC letting the pipeline touch that namespace. See
[examples/02-standard-ci.yaml](examples/02-standard-ci.yaml) for the complete version of
where you are at this point.

## 6. Important: re-run `helm upgrade` after changing `pipelines:`

This is the single most common point of confusion, so it gets called out here too, not
just in the reference doc: **editing `pipelines:` and pushing has two independent
effects.** The git-rooted first stage picks up your change automatically (PaC reads
`.tekton/` fresh on every push). Every stage *after* that is wired up via Trigger
objects that only update when someone runs `helm upgrade` for your app's chart release
with the new `cicd.yaml` as values. If you add/remove/reorder event-chained steps and
they don't seem to fire, this is almost always why - see
[cicd-yaml-reference.md](cicd-yaml-reference.md#multi-stage-pipeline-flows) for the full
explanation.

## Where to go next

- Want promotion to staging with a human-reviewed release? See
  [examples/04-multi-env-promotion.yaml](examples/04-multi-env-promotion.yaml).
- Want PR builds that don't touch your dev environment? See
  [examples/03-pr-validation.yaml](examples/03-pr-validation.yaml).
- Want the full tour of what the platform can do (governance gates, ephemeral
  environments, notifications, tracing)? See [features.md](features.md).
- Want every field, fully explained? See [cicd-yaml-reference.md](cicd-yaml-reference.md).

## If nothing happens

- Check the schema first: `yajsv -s schemas/cicd.schema.json <(yq -o=json . cicd.yaml)`.
  A schema-invalid `cicd.yaml` fails before anything runs, and depending on what's wrong
  you may not get any GitHub status at all for the attempt.
- Check that `cicd.yaml` is actually at your repo root, on the branch you pushed to.
- If you just changed `pipelines:` and an event-chained (non-first) stage isn't firing,
  see step 6 above - this is the most common cause by far.
- If a *later* stage never seems to fire even after a `helm upgrade`, double check the
  event-chaining rule: a stage only chains from the stage immediately before it in your
  `steps:` list, not from anywhere else in the flow. Reordering steps changes what
  triggers what.
