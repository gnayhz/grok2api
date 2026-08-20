.PHONY: run swagger verify verify-full fuzz

CONFIG ?= $(CURDIR)/config.yaml

run:
	cd backend && GOCACHE=$(CURDIR)/.gocache go run ./cmd/grok2api --config "$(abspath $(CONFIG))" $(RUN_ARGS)

swagger:
	cd backend && GOCACHE=$(CURDIR)/.gocache go run github.com/swaggo/swag/cmd/swag@v1.16.6 init \
		-g main.go \
		-d cmd/grok2api,internal/transport/http \
		--parseInternal \
		--output docs \
		--outputTypes go,json,yaml

# One-shot verification matrix (see scripts/verify.sh).
verify:
	scripts/verify.sh fast

verify-full:
	scripts/verify.sh full

# Explicit fuzzing engines (30s per target); seeds run in every verify-full tier.
fuzz:
	scripts/verify.sh fuzz
