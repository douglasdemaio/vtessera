package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/douglasdemaio/vtessera/mcp/internal/mcpserver"
	"github.com/douglasdemaio/vtessera/mcp/internal/vtessera"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	var (
		baseURL  = flag.String("vtessera", envOr("VTESSERA_BASE_URL", "http://localhost:8080"), "base URL of the vtessera marketplace")
		timeout  = flag.Duration("timeout", 15*time.Second, "how long one marketplace call may take")
		listenOn = flag.String("listen", envOr("VTESSERA_MCP_ADDR", ""), "serve streamable HTTP on this address instead of stdio; empty means stdio")
	)
	flag.Parse()

	client, err := vtessera.NewClient(*baseURL, *timeout)
	if err != nil {
		// Refused at startup rather than on the first tool call. A server that
		// answers every tool with a configuration error is harder to diagnose than
		// one that never started.
		log.Fatalf("vtessera-mcp: %v", err)
	}
	// Checked before serving, for the same reason. The marketplace being
	// unreachable is worth knowing about now, and a proxy in front of this server
	// would otherwise turn it into a per-call timeout.
	checkMarketplace(client)

	server := mcpserver.New(client, *baseURL)
	if *listenOn == "" {
		if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			log.Fatalf("vtessera-mcp: %v", err)
		}
		return
	}
	serveHTTP(server, *listenOn)
}

// serveHTTP exposes the same tool set over streamable HTTP for a hosted server.
//
// Only /mcp is registered. There is no health endpoint here on purpose: an operator
// can tell whether this process works by calling a tool, and a second endpoint
// reporting a different notion of health than the marketplace does is one more
// thing that can disagree with the truth.
func serveHTTP(server *mcpserver.Server, addr string) {
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server.Server }, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           probeReporting(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()

	log.Printf("vtessera-mcp %s serving streamable HTTP on %s/mcp", mcpserver.Version, addr)
	// An error here means the listener could not serve at all, which is fatal in the
	// same way a marketplace that will not start is fatal.
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("vtessera-mcp: %v", err)
	}
}

// probeReporting answers a health probe's GET with 200 before the MCP handler
// sees it, because the MCP handler rejects anything that is not POST and a
// directory that checks this endpoint by GET must be able to learn it is up.
// When agent-ai-tool.com carries mcp_endpoint_url it probes it with a GET and
// treats a non-2xx answer as a dead endpoint; a POST-only MCP path would
// otherwise report "withheld, it did not answer a recent health check" for a
// server that answers every real MCP call. The body is a status line only, so a
// client that speaks MCP still POSTs to work.
func probeReporting(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, "vtessera-mcp is serving; speak MCP over POST /mcp\n")
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkMarketplace reports whether the marketplace is answering, once, at startup.
func checkMarketplace(client *vtessera.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	health, err := client.Health(ctx)
	if err != nil {
		log.Printf("vtessera-mcp: warning: %v is not answering yet: %v", client.BaseURL(), err)
		return
	}
	// The verification key is logged because it is the identity behind every
	// attestation this server can report, and an operator comparing two deployments
	// needs to see whether they are the same marketplace.
	log.Printf("vtessera-mcp %s: %s answering, verificationKey %s, sandbox %t",
		mcpserver.Version, health.Status, health.VerificationKey, health.Sandbox)
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
