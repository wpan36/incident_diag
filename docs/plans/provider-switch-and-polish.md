# Provider Switch and Polish

**Tier B spec (M33).** The last milestone.

## Problem

Two claims are still unproven and one document is still a skeleton. The agent runtime is said
to be vendor-independent and has only ever run against DeepSeek; the README has carried
`TODO(M33)` markers since M1.

## Technical Plan

### The switch test exercises a tool call

`LLM_BASE_URL` and `LLM_MODEL` pointed at a second OpenAI-compatible provider, and a run that
**calls a tool**, not one that merely gets a reply. ADR 0006's claim is that the runtime does
not depend on a vendor, and the part most likely to differ between providers is native tool
calling — which is exactly what S7 depends on and what a "does the endpoint answer" check
would miss.

No new account: **SiliconFlow already holds this project's embedding key** and hosts chat
models, so the second provider is a `LLM_BASE_URL`, a `LLM_MODEL` and the key that is already
in `.env`.

It is opt-in through `TEST_ALT_LLM_BASE_URL`, skipping when unset, like every other billed
test here.

### The README is written for someone who has never seen this

What it is, what problem it solves, how to run it, the architecture in one diagram, and a
demo walkthrough with screenshots. No internal trade-offs: those live in `docs/adr/`, and a
README that argues with itself is not an introduction.

The claims it makes have to be ones this repository can support. `answer.delta` was cancelled
(ADR 0010), hybrid retrieval was cancelled (ADR 0007), and the agent is read-only — the
README says what exists.

### The demo script

`make demo`, or a documented sequence: bring the stack up, upload the corpus, inject a fault,
file the incident, open the page. It is what produces the screenshots, and it is the first
thing that breaks when something regresses, which makes it worth having in the repository
rather than in someone's shell history.

### Finishing the documents

`docs/architecture.md` has carried `TODO` markers since M1 and now describes a system that
exists. `docs/rag-eval.md` and `docs/agent-eval.md` are linked from the README as the
project's own measurements.

## Alternatives

**A third provider.** More convincing and no more informative: two is what proves the
runtime is not wired to one.

**Screenshots taken by hand without a script.** Faster once, and they go stale silently.

**Keeping the README short and pointing at `docs/`.** Respects the reader's time and fails
the one job a README has, which is to make someone want to read further.

## Verification

- `make test-integration` with `TEST_ALT_LLM_BASE_URL` set: a run against the second provider
  reaches a terminal status having made at least one tool call.
- The README's instructions followed on a clean checkout, by someone doing exactly what it
  says and nothing else.

## Known limitations, accepted

**"Vendor-independent" is proven for two OpenAI-compatible providers**, not for providers
without native tool calling, which cannot run this agent at all (S7).
