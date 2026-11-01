# ADR-0031: Previews through glidepath-api, beside the `preview` label

*Status: Proposed (2026-11-01). Follow-on 6 of the Glidepath API spec's phase 3
(`POST|DELETE /v1/apps/{app}/previews`, tier T1, Ground only). Nothing here is built.*

## Context

A preview today is a PR labelled `preview` ([ephemeral-environments.md](../ephemeral-environments.md)):
- The app's `<app>-pr-envs` ApplicationSet uses a `pullRequest` generator, filtered to that label and re-polled
  every 180 s. Its template deploys `<ground>/<owner>/<app>-pr:<12-char head sha>` into `app-<app>-pr-<number>`.
- The image comes from the app's `pull_request` flow, which runs on the same label.
- Removing the label or closing the PR deletes the Application, and the namespace goes with it.

The spec wants the same thing as an API call, so an agent or Tower can create, extend or delete a preview. glidepath-api
holds no GitHub write identity, so it cannot add the label. That is deliberate, as for promotion. It needs another
way to tell the ApplicationSet that a preview exists, and a way to end one that the label path does not have: a TTL.

## Decisions

1. **A preview record is a ConfigMap in the app's `-cicd` namespace.** glidepath-api has no database, and release
   records are ConfigMaps already. Each record is named `preview-pr-<number>` and labelled
   `hangar.io/subcomponent=preview`. Its data:
   - `number` and `headSha`;
   - `expiresAt`;
   - `requestedBy`, the caller and the person they act for.

   glidepath-api's ClusterRole gains `create`, `update` and `delete` on ConfigMaps with that label's names only.
   Tower and agents never write the records directly.

2. **The API:**
   - `POST /v1/apps/{app}/previews` with `{pr, ttlHours}`. glidepath-api reads the PR's head SHA with its read token.
     It answers 201, or 200 on a replay with the same `Idempotency-Key`. A second POST for the same PR extends the
     preview: the new `expiresAt` is now plus `ttlHours`, capped at a maximum.
   - `DELETE /v1/apps/{app}/previews/{pr}`.
   - `GET /v1/apps/{app}/previews`. It lists API previews and label previews alike, from the Applications labelled
     `hangar.io/ephemeral-env=true`.

   Tier T1 means a new verb, `preview`, on `environments` in the app's namespace, in the operator role. The calls
   take `dryRun`, and a delegate's write must name its person, as every write does. Defaults: 24 h TTL, 72 h maximum,
   and at most 3 API previews per app.

3. **The ApplicationSet gets a second generator: Argo CD's plugin generator, served by glidepath-api.** The
   ApplicationSet controller calls `POST /internal/previews/getparams.execute` with the app name as its input.
   glidepath-api answers with the unexpired records as `{number, head_sha}`. Those are the fields the
   `pullRequest` generator gives the template, so the template does not change, and an expired record drops out of
   the list. The two generators' outputs are unioned. A PR that is both labelled and requested yields one
   Application name, so the API refuses a request for a labelled PR with a 409.

   The plugin generator reads its bearer token from a Secret in the controller's namespace. That token is a
   `kubernetes.io/service-account-token` Secret for a ServiceAccount `glidepath-preview-generator`, and
   glidepath-api TokenReviews it, as it does every caller. This is the one stored credential in the design. It only
   reads the preview list, and deleting the Secret revokes it.

   The alternative without a stored credential is the cluster-decision-resource generator over a new CRD. It
   needs a CRD, it has to be in the controller's namespace, and its list items must carry an Argo CD cluster name.

4. **The image must already exist.** A PR's image is built only by its `pull_request` flow. glidepath-api checks the
   Ground registry for `<app>-pr:<sha12>` and answers 409 `GP-API-409` without one. The hint names the missing
   build. "Build a branch" (`POST /v1/apps/{app}/builds`, also phase 3) is a separate follow-on. Until it exists, an
   API preview works for a PR whose image was built at some point.

5. **Expiry needs no sweeper.** Once a record expires, the generator omits it and Argo CD deletes the Application,
   cascading the namespace. A CronJob deletes records 24 h after they expire. The existing TTL sweep stays the backstop
   for namespaces.

6. **Clearance:** `glidepath.preview` (T1) maps create, extend and delete onto these calls, after this API is live.

## Consequences

- An agent or a person in Tower can give a PR a preview without the label, and it ends by itself.
- Previews still need the PR's image. Until builds exist, the API is weaker than the label, which also builds.
- Previews keep running on the tenant Argo CD instance (`argocd-apps`). Its ApplicationSet controller must reach
  glidepath-api's Service in `platform-system`.

## Open questions

1. Is the generator's stored ServiceAccount token acceptable (decision 3)? The alternative is the CRD route.
2. Are 24 h default, 72 h maximum and 3 previews per app right?
3. Should `POST /previews` refuse when there is no image (decision 4), or should it wait for "build a branch" and
   do both?
