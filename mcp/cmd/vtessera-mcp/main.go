package main

import (
	"context"
	"encoding/json"
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
// Three things are registered and nothing else. POST /mcp is the MCP transport.
// The MCP Server Card answers a GET at /mcp/server-card, which is the location the
// extension reserves relative to the transport URL, so a discovering client finds
// identity and transport without speaking MCP. Everything else, including a GET of
// /mcp itself, answers the health-probe status line: a directory that checks the
// endpoint with GET must be able to learn it is up, and the MCP handler rejects
// anything that is not POST.
func serveHTTP(server *mcpserver.Server, addr string) {
	mux := newMux(server)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
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

// newMux lays out the hosted routes. It is separate from serveHTTP so the routing
// rules can be exercised without a listener.
func newMux(server *mcpserver.Server) *http.ServeMux {
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server.Server }, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.HandleFunc("GET /mcp/server-card", serveServerCard(server))
	mux.HandleFunc("GET /mcp", serveProbe)
	mux.HandleFunc("/", serveProbe)
	return mux
}

// serveServerCard answers the discovery document for the host it was asked on.
//
// The transport URL is built from the request rather than configured, because this
// process is behind a proxy and cannot read its own public address; a client that
// fetched the card from a hostname is told to connect back to that same hostname.
// The document is advisory, so trusting the Host header here adds no authority: it
// changes only what the fetching client would already believe.
func serveServerCard(server *mcpserver.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(server.Card(transportURL(r))); err != nil {
			log.Printf("vtessera-mcp: writing the server card: %v", err)
		}
	}
}

// transportURL is where a client should send MCP after reading the card.
func transportURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/mcp"
}

// serveProbe answers a health probe's GET with 200, because a directory that
// checks this endpoint by GET treats a non-2xx answer as a dead endpoint; a
// POST-only MCP path would otherwise report "withheld, it did not answer a recent
// health check" for a server that answers every real MCP call. The body is a
// status line only, so a client that speaks MCP still POSTs to work.
func serveProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, "vtessera-mcp is serving; speak MCP over POST /mcp\n")
	}
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
