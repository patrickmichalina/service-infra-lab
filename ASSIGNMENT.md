# Take-home: the platform team is the bottleneck

**Role:** Staff Infrastructure Engineer
**Time budget:** about one working day. Stop at that point even if you are not finished. What you
chose to do first, and what you consciously left out, is part of what we assess.
**AI tools:** allowed and expected. We will ask about the decisions, not about who typed.
**Follow-up:** a 75-minute session. You present for about 20 minutes, extend your Part C code with
us for about 15, then we discuss.

## The problem

We run about 35 services across 10 repositories, owned by four teams, on EKS in two
environments, staging and production. Deployments are gitops: Argo CD applies what a central
`platform-gitops` repository says.

Every time a team adds a service, or changes what one needs from the infrastructure (a new
bucket, a new secret, a new port open from another namespace), they file a request and a
platform engineer writes a five-file pull request in `platform-gitops` on their behalf: an Argo
CD `Application`, an ECR repository, an IAM role bound to the pod's ServiceAccount, a
`NetworkPolicy`, and an `ExternalSecret`. `inputs/CURRENT-STATE.md` describes this, and
`inputs/requests/` holds four real requests, as they arrived.

The platform team is two people. The median request waits four days. Service teams do not
understand the infrastructure their own service runs on, because they never touch it, and the
platform team does not understand the service, so the two sides negotiate IAM policies by
chat. Mistakes are found in production, because the hand-written files are reviewed by the
same two people who wrote them.

**Design software that removes the platform team from the path of a routine change while
keeping the rules in `inputs/CURRENT-STATE.md` holding.** Then build the part of it you think
is the core.

We have deliberately not described a solution. The shape is yours: what the engineer writes,
what checks it, what renders it, where it runs, and what the platform team still does.

## Part A: design (about three hours)

A design document, three to five pages, that a platform team of two could build from. Whatever
shape you choose, we need to be able to see:

1. **What the engineer authors**, and what they never have to know. Show it concretely for at
   least one of the four requests.
2. **Where each rule in `CURRENT-STATE.md` is enforced**: at authoring, at review, at render,
   at admission, or in the cloud provider itself. Say why there. A service repository is
   writable by people the platform team does not review, so treat it as untrusted input.
3. **How a change travels** from the engineer's commit to objects applied in two clusters,
   including how a change gets to soak in staging before it reaches production, and what the
   unit of promotion is when image, chart and infrastructure can change independently.
4. **Failure modes.** At least: a change that passes every check and is rejected downstream;
   a change that removes a permission the running pod depends on; the same setting defined in
   two places.
5. **Migration.** Thirty-five services already run from hand-written files. How they get onto
   the new path without an outage, in what order, and how you would know it is safe to delete
   the old files.
6. **What you cut.** What a complete version would also do, and why it is not in day one.

Markdown, with diagrams where they help. Mermaid is fine.

## Part B: build (about three hours)

Build the core of your design: the piece that, if it is wrong, nothing else matters. For most
designs that will be the thing that takes the engineer's input and produces the applied objects,
with the rules enforced. If you believe the core is somewhere else, say so and build that.

Minimum bar, whatever you build:

- It runs from the command line against files in this directory, with no network and no
  credentials. Environment-specific facts come from `inputs/platform-config/<env>.yaml`.
- It handles all four requests in `inputs/requests/`, in both environments. Some requests should
  be refused, in whole or in part. Your tool's verdict on each, and the reason it gives the
  engineer, is part of the deliverable. An engineer must be able to act on the reason without
  asking the platform team.
- Output is deterministic.
- It has tests. One per rule you enforce is the bar.

Any language. No framework needed. Please do not spend time on packaging.

## Part C: the promotion gate (about two hours)

A smaller, separate piece of software. It does not depend on Parts A and B and is assessed on its
own. Your design may use it for R6, but it does not have to.

R6 says a change reaches prd only after it has been live in stg. Something has to decide whether a
given revision of a service has soaked long enough. Write that decision as a library with a thin
command-line wrapper.

**Input.** `inputs/events/deploy-events.jsonl` is a stream of deployment events, one JSON object
per line, as a consumer would receive them:

```json
{"id":"e-010","at":"2026-10-08T09:00:00Z","env":"stg","service":"settlement-writer","kind":"deployed","revision":"r7"}
```

| kind | meaning |
|---|---|
| `deployed` | `revision` is now the live revision of `service` in `env`, synced and healthy |
| `degraded` | the pods of `revision` in `env` went unhealthy |
| `recovered` | the pods of `revision` in `env` are healthy again |

The stream is delivered **at least once** and is **not in time order**: events from different
producers are merged, and the same event can arrive more than once. `degraded` and `recovered`
are about the revision they name, which is not necessarily the one live at that moment.

**The rule.** A revision is promotable to prd at time `now` if it has been the live, healthy
revision of that service in stg, without interruption, for at least `promotion.soakMinutes`
(`inputs/platform-config/prd.yaml`), ending at `now`. Going unhealthy, or being replaced by
another revision, interrupts it. A revision that comes back later starts again.

**Output.** For each line of `inputs/events/queries.txt` (`now`, service, revision), give one of
three verdicts and a reason the engineer can act on without asking anyone:

- `PROMOTABLE`
- `WAIT`: it is on track, and you say when it becomes promotable if nothing changes
- `BLOCKED`: waiting alone will not make it promotable, and you say what will

Minimum bar:

- `now` is an input. Nothing reads the wall clock, so the same input always gives the same output.
- Tests, including one for each way the input can mislead a naive implementation. Finding those
  is part of the task.
- The input has defects. Decide what your gate does with each and say why. Do not drop anything
  silently.

Use whatever language you write best. It need not match Part B.

In the follow-up we will ask you to change the rule live, with your tests as the safety net, so
write it the way you would want to find it.

## What we want back

1. The design document.
2. The tool, its tests, and its output for all four requests in both environments.
3. The promotion gate, its tests, and its output for every line of `queries.txt`.
4. Your working notes, however rough, including where an AI assistant was wrong and you
   overrode it.

## How we evaluate

Roughly in this order:

- **Judgment about the trust boundary.** Where you put each rule, and why, matters more than
  how many rules you wrote.
- **Whether the design would survive contact** with 35 services, four teams, and a platform
  team of two. Migration and promotion are where this shows.
- **Whether the software is the right core**, and whether its refusals tell an engineer what to
  do.
- **Software engineering in Part C.** The right verdicts, a data model that makes the rule
  obvious, tests that would catch a regression, and code someone else could safely change.
- **Honesty.** What you did not do, what you are unsure of, and what you would need to find out.
- **Code quality proportional to the time.** Clear, tested, no cleverness.

Send a zip or a repository link.
