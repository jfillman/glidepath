# ADR-0030: Durable event fan-out over AMQP, beside the SSE stream

*Status: Proposed (2026-10-31). Design only: nothing here is built. Extends the event stream described in
[api.md#events](../api.md#events), which stays.*

## Context

glidepath-api serves Glidepath's CDEvents as a server-sent event stream (`GET /v1/events`) with resume. The events
come in through the broker's tap, and each replica holds them in memory: the last 2000, none older than 24 h. That
was chosen over AMQP on 2026-10-29 because the only subscriber was Autopilot's event bridge, and a broker for one
subscriber cost more than it bought. `routingKey` was given AMQP topic semantics so a bridge could be added later
without subscribers changing what they match.

What the in-memory stream cannot do:

- **Survive a full restart.** When every replica restarts at once, the buffer is empty. A subscriber resuming from
  before that silently starts at the oldest event the buffer holds.
- **Hold events for a subscriber that is down.** Autopilot's bridge misses whatever arrived while it was down longer
  than the buffer reaches back. An unattended agent trigger is exactly the kind of consumer that should not miss one.
- **Share load.** Every subscriber holds its own stream to one replica, and a replica serves at most 100.
- **Acknowledge.** A subscriber cannot say "done with this one". It dedupes by id and hopes.

hangar-dev already runs the RabbitMQ Cluster Operator and the Messaging Topology Operator in `rabbitmq-system`. One
InfraService, `skyport-broker`, already uses them.

## Decisions

1. **A RabbitMQ topic exchange carries every event, beside the SSE stream.** The exchange is `glidepath.events`, in a
   `RabbitmqCluster` named `glidepath-events` in `platform-system`. The chart declares the cluster, exchange, users,
   permissions, queues and bindings as Topology Operator resources. The lab runs 1 replica and production 3.
   `GET /v1/events` stays. Tower and ad-hoc readers keep it. AMQP is the durable path for unattended consumers.

2. **glidepath-api publishes, once per event, from the replica the tap reached.** The tap (`events.Tap`) is where an
   event is verified and given its id. That replica publishes it with:
   - the routing key `routingKey` (`<app>.<subject>.<predicate>`);
   - `message_id` set to the event id;
   - headers `app` and `type`;
   - the same envelope SSE sends, as the body.

   Peers that receive the event by forwarding do not publish. Publishing uses publisher confirms. glidepath-api is
   the only user with `write` on the exchange.

3. **The broker is the durability boundary.** An event is durable once the broker confirms it. Before that,
   glidepath-api retries from a bounded in-memory queue (1000 events). An event that does not fit, or that is still
   unconfirmed when the pod stops, is dropped and counted in `glidepath_api_amqp_dropped_total`. glidepath-api does
   not get an outbox table. It has no database, and the SSE buffer has the same best-effort guarantee today. A broker
   that is down never fails the tap or the SSE stream. `GlidepathApiAmqpPublishFailing` alerts on it.

4. **Each consumer has its own durable queue, declared in Git.** A consumer gets a quorum queue
   `glidepath.events.<consumer>`, bound with its binding keys, plus a RabbitMQ user with `read` on that queue only.
   Its entry in the chart's `api.events.amqp.consumers` values lists:
   - the consumer name;
   - its binding keys, such as `*.release.#`;
   - the apps it may see.

   The chart renders bindings only for those apps' keys (`<app>.#`, intersected with the requested keys). The
   per-app authorization SSE does with a SubjectAccessReview is done here once, at review time, by whoever merges
   the values change. A queue has:
   - `x-message-ttl` of 24 h, matching the SSE buffer's reach;
   - `x-max-length` of 10000, with `reject-publish-dlx` overflow;
   - `delivery-limit` 5;
   - the dead-letter exchange `glidepath.events.dead`, with one queue per consumer.

5. **Delivery is at least once.** A consumer acks after it has acted. It dedupes by `message_id`, as Autopilot's
   bridge already does by event id. Order is per queue as published. It is not guaranteed across a publisher retry,
   which matches SSE replicas today.

6. **Credentials are per consumer, never shared, and never in Git.** The Topology Operator's `User` generates each
   consumer's credentials into a Secret in `platform-system`. External Secrets copies it into the consumer's
   namespace (for Autopilot, `clearance-system`). The secrets are rotated by deleting the Secret.

   A later option, not part of this ADR, is RabbitMQ's OAuth 2 plugin with Kubernetes ServiceAccount tokens as
   JWTs. That removes the stored password. It needs the cluster's ServiceAccount issuer to be reachable by the
   broker.

7. **Autopilot moves first; SSE stays its fallback.** The event bridge gets an AMQP reader behind a setting. When the
   queue is unreachable it falls back to the SSE stream it has today. Its trigger matching, `amqp_topic_match` on
   `bindingKey`, does not change: the same keys become queue bindings.

## Consequences

- An unattended trigger no longer misses an event because its subscriber or every glidepath-api replica restarted.
  It still misses one that was never confirmed (decision 3).
- One more stateful component runs on the lab's least stable hardware. hangar-dev's control plane restarts its
  controllers often. RabbitMQ needs its own PVC. The RabbitMQ operator has needed a restart after an API server
  restart on hangar-dev before.
- Two paths deliver the same events. A consumer must use one. Tower stays on SSE.
- Who sees which app's events is set in Git at review time, not per call. A consumer granted an app keeps getting its
  events until the values change, even if its Kubernetes grant is revoked.

## Alternatives considered

- **Keep SSE only (status quo).** It is cheapest and adequate while Autopilot is the only unattended consumer and
  loss on a full restart is acceptable. This is the right answer until a second unattended consumer appears, or a
  missed trigger costs something.
- **Persist the SSE buffer** (a PVC or Postgres behind glidepath-api). It fixes restart loss but not "subscriber down
  longer than the buffer", acks or load sharing. It also makes glidepath-api stateful.
- **NATS JetStream.** It has durable consumers and is lighter than RabbitMQ. It is not on the cluster, and its
  subject wildcards (`*`, `>`) differ from the AMQP keys Autopilot triggers already use.
- **Kafka.** Far more than this volume needs.
- **Knative Eventing broker.** It is CloudEvents-native, but it is not on the cluster and would be a second eventing
  system beside the Tekton-based broker.

## Phases, if accepted

1. The broker: the `RabbitmqCluster`, the exchange, the dead-letter exchange, glidepath-api's user, and
   `api.events.amqp.enabled` (default off).
2. The publisher in glidepath-api: confirms, the bounded retry queue, metrics and alerts. Verify one confirmed
   publish per tapped event across both replicas.
3. Consumers: chart values, rendered queues, bindings and users, and the Secret copied to `clearance-system`.
4. Autopilot's AMQP reader with SSE fallback. Verify a trigger fires for an event published while the bridge was down.

## Open questions

- Should production run its own broker, or should the lab's be reachable from hangar-prod? glidepath-api only runs
  on hangar-dev today, so phase 1 needs only hangar-dev.
- Is a 24 h queue TTL right for a trigger, or should an event older than its trigger's purpose be dropped sooner?
