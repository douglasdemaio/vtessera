BINARY := bin/vtessera
PKG := ./...
GO ?= go
IMAGE ?= vtessera:local

# The validator-backed suite is build-tagged so the hermetic suite never needs a
# running validator. VTESSERA_TEST_RPC_URL points it at a local test validator.
.PHONY: all build run test race test-solana test-devnet validator validator-off vet fmt lint tidy clean smoke quickstart image image-run fly-deploy fly-verify preflight-live mcp-build mcp-test mcp-fmt mcp-vet mcp-tidy

# mcp/ is a separate module (see AGENTS.md), so ./... does not reach it. Its checks
# are wired in here rather than left to memory: a nested module that nothing builds
# is a module that rots quietly, and this one talks to the service's public API.
all: fmt vet test build mcp-fmt mcp-vet mcp-test mcp-build

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

# Completes a full trade against the public devnet cluster and checks the cap
# refusals there. It touches the network but moves no value: the trade settles
# off-chain and no key with a balance is used. `make test` stays hermetic, which
# is why this is behind its own tag. Override the endpoint with:
#
#   make test-devnet RPC=https://api.devnet.solana.com
test-devnet:
	$(GO) test -tags devnet -timeout 15m -run Devnet ./internal/e2e/

# Checks a cluster for real without starting the service. It runs the same
# verification the service performs at boot, so it is the only way to find out
# whether an endpoint is the cluster it claims to be before a deploy depends on
# it. Override any of the four settings:
#
#   make preflight-live CLUSTER=devnet RPC_URL=https://api.devnet.solana.com
#   make preflight-live CLUSTER=mainnet-beta RPC_URL=https://solana-rpc.publicnode.com MAINNET_ACK=1
#
# mainnet-beta requires MAINNET_ACK=1 because settlement there moves real value.
preflight-live: build
	./$(BINARY) --preflight-only \
		--cluster $(or $(CLUSTER),devnet) \
		--rpc-url $(or $(RPC_URL),https://api.devnet.solana.com) \
		$(if $(filter mainnet-beta,$(CLUSTER)),--mainnet-ack $(or $(MAINNET_ACK),1),) \
		--session-secret $(or $(VTESSERA_SESSION_SECRET),0123456789abcdef0123456789abcdef)

MCP_DIR := mcp
MCP_BINARY := $(MCP_DIR)/bin/vtessera-mcp

mcp-build:
	cd $(MCP_DIR) && $(GO) build -trimpath -o bin/vtessera-mcp ./cmd/vtessera-mcp

mcp-test:
	cd $(MCP_DIR) && $(GO) test ./...

mcp-vet:
	cd $(MCP_DIR) && $(GO) vet ./...

mcp-fmt:
	cd $(MCP_DIR) && test -z "$$($(GO) fmt ./...)" || { echo "mcp/ needs gofmt: run \'cd mcp && go fmt ./...\'"; exit 1; }

mcp-tidy:
	cd $(MCP_DIR) && $(GO) mod tidy

vet:
	$(GO) vet $(PKG)

fmt:
	$(GO) fmt $(PKG)

tidy:
	$(GO) mod tidy

smoke: build
	./scripts/smoke.sh $(BINARY)

# Runs both quickstarts against a throwaway sandbox. Deliberately not part of
# `all`: it needs python3 (with cryptography) and node 22.18+, and the hermetic
# suite must not start depending on two more runtimes. The guides are the API's
# front door, so this is how they are kept honest rather than left to rot.
quickstart: build
	./scripts/quickstart.sh $(BINARY)

# podman defaults to the OCI image format, which has no HEALTHCHECK field, so it
# drops the directive with a warning and the container reports no health status.
# docker's own format keeps it. Requesting docker's format explicitly is correct
# under both, and the alternative — losing the healthcheck under podman — fails
# quietly, which is exactly the kind of thing that should not be left to memory.
image:
	podman build --format docker -t $(IMAGE) -f Containerfile .

# A local run against a persistent volume. The session secret is regenerated per
# run, which invalidates old sessions; set VTESSERA_SESSION_SECRET to keep them.
# Set VTESSERA_PUBLIC_BASE_URL to make the agent card advertise a reachable URL.
image-run: image
	podman run --rm -it -p 8080:8080 -v vtessera-data:/data \
		-e VTESSERA_SESSION_SECRET="$${VTESSERA_SESSION_SECRET:-$$(openssl rand -hex 32)}" \
		-e VTESSERA_PUBLIC_BASE_URL="$${VTESSERA_PUBLIC_BASE_URL:-}" \
		$(IMAGE)

# Deploy to Fly.io from fly.toml. The one-time setup (app, volume, secrets) is
# in docs/deploy.md; this is the part you run on every change.
#
# --ha=false is deliberate: this service owns its state on one volume, so Fly's
# default of two machines would create two marketplaces with two signing keys.
fly-deploy:
	fly deploy --ha=false

# The verification key printed here is the marketplace identity. Run it before
# and after a deploy: if it changed, the volume is gone.
fly-verify:
	@curl -fsS "https://$$(sed -n 's/^app *= *"\(.*\)"/\1/p' fly.toml).fly.dev/healthz" \
		| python3 -c "import json,sys; print('verificationKey:', json.load(sys.stdin)['verificationKey'])"

clean:
	rm -rf bin
