.DEFAULT_GOAL := check

GO ?= go

# Every milestone must pass `make check` before it is committed.
.PHONY: check
check: fmt-check vet test race

.PHONY: fmt
fmt:
	gofmt -w .

# Fails (listing offenders) instead of rewriting, so CI and commits can rely on it.
.PHONY: fmt-check
fmt-check:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt needed for:"; echo "$$out"; exit 1; \
	fi

# The go tool treats "./... matched no packages" as an error, which would break
# `make check` on a repo that has no Go code yet. Nothing to do is not a failure.
HAVE_PKGS = [ -n "$$($(GO) list ./... 2>/dev/null)" ]

.PHONY: vet
vet:
	@if $(HAVE_PKGS); then $(GO) vet ./...; else echo "no Go packages yet; skipping vet"; fi

.PHONY: test
test:
	@if $(HAVE_PKGS); then $(GO) test ./...; else echo "no Go packages yet; skipping test"; fi

.PHONY: race
race:
	@if $(HAVE_PKGS); then $(GO) test -race ./...; else echo "no Go packages yet; skipping race"; fi

# Integration tests need the docker compose infrastructure running. They skip
# cleanly when it is absent, so `make test` stays green either way.
.PHONY: test-integration
test-integration:
	$(GO) test -tags=integration ./...

.PHONY: tidy
tidy:
	$(GO) mod tidy
