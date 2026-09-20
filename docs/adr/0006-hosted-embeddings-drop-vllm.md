# ADR 0006: Drop vLLM; use hosted embeddings

**Status:** accepted
**Date:** 2026-09-20
**Supersedes:** [ADR 0002](0002-vllm-serves-embeddings.md)

## Context

ADR 0002 gave vLLM a real job — serving `BAAI/bge-m3` locally — on the grounds that the
6 GB laptop GPU could do embedding inference well even though it could not host a chat
model worth building an agent against.

That reasoning still holds on its own terms, but it optimized for the wrong thing. This is
a portfolio project, and its most valuable property is that someone else can clone it and
watch it work. Requiring an NVIDIA GPU means most people who open the repository cannot
run it, and the local inference server is a substantial piece of the Compose stack that
exists to demonstrate one skill rather than to make the product work.

DeepSeek, which serves the chat model, has no embeddings endpoint, so removing vLLM still
leaves RAG needing an embedding provider.

## Decision

Remove vLLM from the project entirely.

- **Chat** stays on DeepSeek over its OpenAI-compatible API.
- **Embeddings** move to SiliconFlow, which hosts `BAAI/bge-m3` behind an
  OpenAI-compatible `/v1/embeddings` endpoint.
- **Provider independence** is demonstrated by switching between two hosted providers
  rather than between hosted and local (M33).

## Reasoning

SiliconFlow serves the same model at the same 1024 dimensions, so nothing downstream
changes: the Elasticsearch `dense_vector` mapping, the chunk structure and the
`internal/embed` client are exactly as specified. The change is `EMBEDDING_BASE_URL`
pointing somewhere else.

The claim worth making was never "I can run vLLM". It was "the agent runtime is not
coupled to a model vendor", and that is proven by a smoke test switching `LLM_BASE_URL`
between two providers — which is cheaper to run, works on any machine, and is something a
reader can reproduce.

The timing is why this is worth doing rather than living with. No Go code referenced vLLM:
M8, the milestone that would have built against it, has not been written. The cost is
editing six documents.

## Consequences

- **The stack runs on any machine.** No GPU, no CUDA, no model weights to download, and
  the Compose file loses a heavy service.
- **Local LLM serving is no longer demonstrated.** This is a genuine loss and the honest
  way to describe the project is that it demonstrates OpenAI-compatible LLM integration
  and vendor independence, not model serving.
- **Two API keys and a network dependency.** Ingestion now fails without connectivity,
  where a local embedding server would have kept working. The embedding client's timeout
  and bounded-retry behaviour matters more than it did.
- **Embedding costs money per document**, though trivially so at this project's volume.
- **The 1024-dimension mapping is unchanged**, so no prior decision about Elasticsearch
  needs revisiting.

## Alternatives considered

**Keep vLLM for embeddings only**, which is what ADR 0002 decided. Rejected: it is the
single thing forcing a GPU requirement on everyone who wants to run the project.

**OpenAI `text-embedding-3-small`.** The most widely recognized option, but a different
dimensionality, an additional billing relationship, and awkward network access from where
this is developed.

**A lighter local server such as Ollama or TEI.** Still a local inference service and
still effectively requires a GPU, so it does not address the reason for the change.
