# ADR 0005: Scope boundaries

**Status:** accepted
**Date:** 2026-09-20

## Context

The project's purpose is to learn and to demonstrate a specific set of technologies, and
to be genuinely runnable while doing so. It is explicitly not a commercial SRE product.
Without stated boundaries, "make it realistic" expands without limit, and the parts worth
building get squeezed out by the parts that are merely expected.

## Decision

The following are out of scope for this project, permanently unless revisited here.

### No user system

No accounts, no login, no RBAC, no multi-tenancy, no billing. These are well-understood
problems that consume a lot of implementation time and teach nothing the project set out
to learn. The system assumes a single trusted operator.

### The agent is read-only

The agent cannot run arbitrary shell commands, cannot modify any system it investigates,
and cannot perform automated remediation. Its tools are read-only and allowlisted at the
MCP server. This is a design constraint, not a first-version limitation to be relaxed
later: an agent that can act on production is a different project with different safety
requirements.

### No high availability

MySQL, Kafka, Elasticsearch and Redis each run as a single node. No replication, no
sharding, no failover. The distributed-systems content worth learning here is idempotency
and backpressure, not cluster operations.

### Front end is functional, not polished

The React UI exists so the system can be used and demonstrated. It is not a design
exercise and should not absorb time that belongs to the backend.

## Consequences

- The system cannot be deployed publicly as-is. There is no authentication on any
  endpoint.
- Demonstrations run on Docker Compose on a single machine.
- Anyone reading the repository should find these boundaries stated rather than inferring
  that they were overlooked — which is the reason this ADR exists.

## Alternatives considered

**Add basic auth to make it deployable.** Rejected for now: it implies a deployment story
the project does not have, and single-user assumptions are baked in elsewhere anyway.

**Let the agent propose and execute remediation behind an approval step.** Genuinely
interesting and a plausible second version, but it changes the safety model substantially
and is not needed to demonstrate anything on the current list.
