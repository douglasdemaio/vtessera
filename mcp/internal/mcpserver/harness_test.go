package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/mcp/internal/vtessera"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpSession is a server under test plus a client that speaks to it over the real
// protocol, with a stand-in marketplace behind it.
//
// The MCP session is opened per call rather than once per test because a server
// session in this SDK is bound to one transport, and a test that reused one would
// be testing a client that had already gone away.
type mcpSession struct {
	t      *testing.T
	market *httptest.Server
	server *Server
}

func newTestServer(t *testing.T, handler http.Handler) *mcpSession {
	t.Helper()
	market := httptest.NewServer(handler)
	t.Cleanup(market.Close)
	client, err := vtessera.NewClient(market.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return &mcpSession{t: t, market: market, server: New(client, market.URL)}
}

// withSession runs fn against a freshly connected MCP client.
func (s *mcpSession) withSession(fn func(*testing.T, *mcp.ClientSession)) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.t.Context(), 30*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := s.server.Connect(ctx, serverTransport, nil)
	if err != nil {
		s.t.Fatalf("connecting the server: %v", err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		s.t.Fatalf("connecting the client: %v", err)
	}
	defer clientSession.Close()
	fn(s.t, clientSession)
}

// call runs one tool and returns its result.
func (s *mcpSession) call(name string, args map[string]any) *mcp.CallToolResult {
	s.t.Helper()
	var res *mcp.CallToolResult
	s.withSession(func(t *testing.T, session *mcp.ClientSession) {
		out, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		res = out
	})
	return res
}

// callExpectingError runs one tool that must refuse.
func (s *mcpSession) callExpectingError(name string, args map[string]any) *mcp.CallToolResult {
	s.t.Helper()
	var res *mcp.CallToolResult
	s.withSession(func(t *testing.T, session *mcp.ClientSession) {
		out, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !out.IsError {
			t.Fatalf("%s returned success, want a refusal", name)
		}
		res = out
	})
	return res
}

// listTools returns the advertised tool set.
func (s *mcpSession) listTools() []*mcp.Tool {
	s.t.Helper()
	var tools []*mcp.Tool
	s.withSession(func(t *testing.T, session *mcp.ClientSession) {
		listed, err := session.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatalf("listing tools: %v", err)
		}
		tools = listed.Tools
	})
	return tools
}

// resultText is the rendered content of a tool result, which is what a model reads.
func resultText(r *mcp.CallToolResult) string {
	var b []byte
	for _, content := range r.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			b = append(b, text.Text...)
		}
	}
	return string(b)
}

// decodeStructured reads the structured output a tool returned.
func decodeStructured(t *testing.T, res *mcp.CallToolResult, dst any) {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool returned an error: %s", resultText(res))
	}
	if res.StructuredContent == nil {
		t.Fatalf("tool returned no structured content: %s", resultText(res))
	}
	encoded, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, dst); err != nil {
		t.Fatalf("decoding structured content into %T: %v", dst, err)
	}
}

// healthResponse answers /healthz the way the service does.
func healthResponse(w http.ResponseWriter) {
	w.Header().Set("content-type", "application/json")
	_, _ = w.Write([]byte(`{
	  "status":"ok","version":"0.1.0-test",
	  "verificationKey":"5LRpM9wpvPfRYuQAC7oNdyaQa6sakpMcnZeR9FS5CgjB",
	  "sandbox":true
	}`))
}

// healthOnly serves a marketplace that answers nothing but /healthz.
func healthOnly() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		healthResponse(w)
	})
}

// offersHandler serves a marketplace whose only other answer is an offer search.
func offersHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			healthResponse(w)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(body))
	})
}
