BINARY := bin/vtessera
PKG := ./...
GO ?= go

# The validator-backed suite is build-tagged so the hermetic suite never needs a
# running validator. VTESSERA_TEST_RPC_URL points it at a local test validator.
.PHONY: all build run test race test-solana validator validator-off vet fmt lint tidy clean smoke

all: fmt vet test build

build:
	$(GO) build -trimpath -o $(BINARY) ./cmd/vtessera

run:
	$(GO) run ./cmd/vtessera --session-secret $(or $(VTESSERA_SESSION_SECRET),0123456789abcdef0123456789abcdef)

test:
	$(GO) test $(PKG)

race:
	$(GO) test -race $(PKG)

validator:
	solana-test-validator --reset --quiet

# The validator holds ~1 GB of RAM and writes gigabytes of ledger. It must not
# outlive the suite that needs it.
#
# `pkill -x` cannot be used here: Linux truncates process names to 15
# characters, so it can never match "solana-test-validator". `pkill -f` does
# match, but this recipe's own shell command line contains the pattern, so it
# would kill itself and take make down with it. Hence: enumerate the matches,
# then skip our own PID.
validator-off:
	@for p in $$(pgrep -f solana-test-validator 2>/dev/null); do \
		[ "$$p" = "$$$$" ] || kill "$$p" 2>/dev/null || true; \
	done
	@echo "test validator stopped"

test-solana:
	$(GO) test -tags solana -timeout 30m ./internal/e2e/

vet:
	$(GO) vet $(PKG)

fmt:
	$(GO) fmt $(PKG)

tidy:
	$(GO) mod tidy

smoke: build
	./scripts/smoke.sh $(BINARY)

clean:
	rm -rf bin
