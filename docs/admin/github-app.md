# The Pipelines-as-Code GitHub App and its two names

Every gate result a PR shows comes from one GitHub App, the one Pipelines-as-Code (PaC) runs as. Two different names
appear on each check, and they are set in different places. Neither has to match the other.

| | The check run's name | The App's name |
|---|---|---|
| Example | `Glidepath / sast-` | `cicd-pac` (slug), shown as the check's author |
| What sets it | PaC's `application-name`, in the `pipelines-as-code` ConfigMap (PaC's namespace, `tekton-pipelines` here), followed by the PipelineRun's `generateName` | whoever creates the GitHub App, when they create it; renaming the App changes its slug |
| Hangar's value | `Glidepath`: Apron's `cluster.yaml` `pipelinesAsCode.applicationName`, rendered into the ConfigMap and the catalog together | whatever the install chose (`cicd-pac` in the lab) |
| Who reads it | the gate tasks, `hack/apply-*-ruleset.sh`, Tower | Tower (signature evidence), the ruleset (`integration_id`, the App's numeric id) |

## The check run's name: `pipelinesAsCode.applicationName`

PaC names every check `<application-name> / <PipelineRun generateName>`, and every gate's `generateName` is `<gate>-`.
Its default `application-name` is `Pipelines as Code CI`; Hangar sets `Glidepath`.

The gate tasks look up their sibling gates' checks by that full name: `wait-for-release-guardrails`,
`detect-bypass-merge`, `release-log-emit`, `promote` and the promotion fan-in in `comment-pr-check-result`, and the
Clearance gate. `promote` also posts the `image-promotion-` check itself, under the same name, so it reads like any PaC
gate. They take the name from the catalog value:

```yaml
# glidepath-catalog values
pipelinesAsCode:
  applicationName: Glidepath                       # must equal the ConfigMap's application-name
  legacyApplicationNames: ["Pipelines as Code CI"] # earlier names, still matched when reading checks
```

**`applicationName` must equal the ConfigMap's `application-name`.** Glidepath does not install PaC (the cluster repo
does) and does not own that ConfigMap. Apron renders both from the one `cluster.yaml` value. A cluster repo that installs
PaC some other way sets both itself. If they differ, the gate tasks still find PaC's checks when the ConfigMap's value
is listed in `legacyApplicationNames`. `promote`'s `image-promotion-` check, though, is posted under `applicationName`,
and a ruleset built from these values requires checks that PaC never posts.

`legacyApplicationNames` keeps a rename safe. A check already posted keeps its name, so the open PRs and their
`/retest` history at the time of a rename carry the old one. The tasks match a gate's check under any of the names. Keep the old name listed until no open release PR has checks under it.

Tower and the Backstage backend do not need the value. They drop everything up to the first ` / ` of a check name to
get the gate id, whatever the application name is.

### Renaming it

1. Ship a catalog whose `pipelinesAsCode` lists the old name in `legacyApplicationNames` and the new one as
   `applicationName`. Readers then accept both names.
2. Change the ConfigMap's `application-name` (Apron: `cluster.yaml` `pipelinesAsCode.applicationName`, then render).
   PaC rereads it; the next check it posts uses the new name.
3. Re-apply the rulesets with the new names (`hack/apply-guardrail-ruleset.sh -f <cluster values>`,
   `hack/apply-agent-gates-ruleset.sh --app-name <name>`): a ruleset still requiring `<old name> / sast-` blocks every
   PR once checks are named `<new name> / sast-`. Where rulesets are enforced, do steps 2 and 3 together. Expect a short
   window in which a PR opened in between needs `/retest`.

## The App's name: `glidepath.gateCommentApps`

Nothing in Glidepath depends on what the App is called. A new Hangar install can create it under any name.

The App's **slug** matters to Tower. The provenance gate's PR comment carries a `<!-- glidepath:signature {...} -->`
line (how the released commit was proven, ADR-0027), and the api-compat gate's carries its verdict. Tower trusts those
lines only in a comment the platform's App posted. Anyone can write a comment that starts with the gate marker. The
Backstage app config names the App by slug:

```yaml
# Backstage app-config
glidepath:
  gateCommentApps: [cicd-pac]   # the PaC GitHub App's slug, github.com/apps/<slug>
```

Set it to your install's App slug. Left unset, or set to the wrong slug, Tower shows no signature evidence on any
release. Renaming the App changes its slug, so update this
value with it.

The rulesets name the App by its numeric id (`--integration-id`, default `4461953`, the lab's App). The id survives a
rename, but a new install's App has its own id: pass it when applying the rulesets
([github-ruleset.md](github-ruleset.md)).
