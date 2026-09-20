# ADR 0002: vLLM serves embeddings; the chat model is DeepSeek

**Status:** accepted
**Date:** 2026-09-20

## Context

The project wants to demonstrate LLM serving with vLLM, and the developer has DeepSeek API
credit. Two facts constrain the decision:

- The development machine has an **NVIDIA RTX A1000 Laptop GPU with 6 GB of VRAM**.
- DeepSeek's API does not offer an embeddings endpoint, so RAG needs an embedding provider
  regardless.

At 6 GB, the largest chat models that fit are roughly 3B parameters at 4-bit quantization.
Models that small are unreliable at multi-step tool calling: structured output degrades
and agent loops fail to converge. Building the agent against one would mean spending
milestones fighting the model instead of building the system.

## Decision

- **Embeddings run locally on vLLM**, serving `BAAI/bge-m3` (1024 dimensions, ~2.3 GB in
  fp16). This comfortably fits in 6 GB. Fallback if it does not work out:
  `Qwen3-Embedding-0.6B`.
- **The chat model is DeepSeek** over its OpenAI-compatible API.
- A separate Compose profile runs a small local chat model (`Qwen2.5-3B-Instruct-AWQ`)
  purely to run a smoke test proving the agent runtime is provider-agnostic.

## Reasoning

This gives vLLM a job it is genuinely good at on this hardware instead of a job it cannot
do. Embedding inference is small, batched and throughput-oriented — exactly what a 6 GB
card handles well — and it is on the critical path of every document ingested, so it is a
real dependency rather than a demo.

Provider independence is still demonstrated, but by the cheap correct means: the runtime
only ever reads `LLM_BASE_URL`, `LLM_API_KEY` and `LLM_MODEL`, and a smoke test proves
that pointing those at a local vLLM server works. That is the claim worth making. Making a
3B model complete a full investigation is a different and much more expensive claim, and
not one this project needs.

## Consequences

- Running the full stack requires an NVIDIA GPU. This is a real barrier to someone else
  reproducing the project, and the README must say so.
- Embedding dimension (1024) is baked into the Elasticsearch mapping. Changing the
  embedding model means reindexing every document.
- The agent's quality depends on a hosted API, so the demo needs network access and
  credit.

## Alternatives considered

**A hosted embedding API** (SiliconFlow, Jina, OpenAI). Simpler and GPU-free, and remains
a drop-in alternative since the client only speaks the OpenAI-compatible protocol. Not
chosen as the default because it would leave vLLM with no real role in the system.

**Drop vLLM entirely.** Rejected: LLM serving is one of the things this project exists to
learn.

**Run the agent on a local model.** Rejected on the hardware grounds above.
