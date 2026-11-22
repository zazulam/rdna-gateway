package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zazulam/rdna-gateway/internal/mcp"
	"github.com/zazulam/rdna-gateway/internal/otlp"
	"github.com/zazulam/rdna-gateway/internal/otlp/otlptest"
	"github.com/zazulam/rdna-gateway/internal/rag"
)

const canary = "CANARY-e2e-91b7-private"

// fakeBackends is an embedder, reranker, Qdrant and SearXNG whose answers all
// quote the canary, as stored memories and web results would.
func fakeBackends(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		p := r.URL.Path
		switch {
		case p == "/v1/embeddings":
			var data []any
			for i := range in["input"].([]any) {
				data = append(data, map[string]any{"index": i, "embedding": []float32{1, 0, 0}})
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]any{"prompt_tokens": 12}})
		case p == "/v1/rerank":
			var res []any
			for i := range in["documents"].([]any) {
				res = append(res, map[string]any{"index": i, "relevance_score": 1.0 / float64(i+1)})
			}
			json.NewEncoder(w).Encode(map[string]any{"results": res})
		case strings.HasSuffix(p, "/exists"):
			w.Write([]byte(`{"result":{"exists":true}}`))
		case strings.HasSuffix(p, "/points/search"):
			w.Write([]byte(`{"result":[{"score":0.9,"payload":{"text":"memory ` + canary + `","source":"memory/1"}},
				{"score":0.8,"payload":{"text":"other ` + canary + `","source":"memory/2"}}]}`))
		case p == "/search":
			w.Write([]byte(`{"results":[{"title":"` + canary + `","url":"https://example.com/` + canary + `","content":"` + canary + `"}]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestToolSpansExportedOverOTLP(t *testing.T) {
	recv := otlptest.New(t)
	exp := otlp.NewExporter(otlp.Config{Endpoint: recv.URL}, "test", "rdnamcp", otlp.Options{BatchTimeout: 10 * time.Millisecond})
	defer exp.Shutdown(context.Background())
	back := fakeBackends(t)
	r := &rag.Retriever{E: rag.NewEmbedder(back, "emb"), R: rag.NewReranker(back, "rr"), S: rag.NewStore(back), Candidates: 5}
	s := &mcp.Server{Name: "rdnamcp", Version: "1", Token: "tok", Tools: toolset(r, back), Tracer: otelTracer{exp},
		Logf: func(string, ...any) {}}
	srv := httptest.NewServer(s)
	defer srv.Close()

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const agentSpan = "00f067aa0ba902b7"
	call := func(id int, tool string, args map[string]any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{
			"name": tool, "arguments": args, "_meta": map[string]any{"traceparent": "00-" + traceID + "-" + agentSpan + "-01"}}})
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Result struct{ Content []struct{ Text string } }
		}
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if len(out.Result.Content) == 0 {
			t.Fatalf("%s: no result", tool)
		}
	}
	call(1, "recall", map[string]any{"query": canary})
	call(2, "remember", map[string]any{"text": "remember " + canary, "topic": canary})
	call(3, "web_search", map[string]any{"query": canary})
	call(4, "fetch_url", map[string]any{"url": "http://127.0.0.1/" + canary})
	call(5, "search_docs", map[string]any{"query": canary, "collection": "docs"})
	exp.ForceFlush(context.Background())

	if raw := recv.Raw("/v1/traces"); raw == "" || strings.Contains(raw, "CANARY") {
		t.Fatalf("exported traces must not carry tool input or output:\n%s", raw)
	}
	spans := recv.Spans()
	byID := map[string]otlptest.Span{}
	var recall otlptest.Span
	for _, sp := range spans {
		byID[sp.SpanID] = sp
		if sp.Name == "tools/call recall" {
			recall = sp
		}
		if sp.TraceID != traceID {
			t.Fatalf("%s is outside the agent's trace: %s", sp.Name, sp.TraceID)
		}
	}
	if recall.ParentSpanID != agentSpan || recall.Kind != 2 || recall.Attrs["gen_ai.tool.name"] != "recall" ||
		recall.Attrs["jsonrpc.request.id"] != "1" || recall.Attrs["mcp.method.name"] != "tools/call" {
		t.Fatalf("recall span: %+v", recall)
	}
	var sawEmbed, sawSearch, sawRerank, sawFetchErr bool
	for _, sp := range spans {
		parent := byID[sp.ParentSpanID]
		switch {
		case sp.Name == "embeddings emb" && parent.Name == "tools/call recall":
			sawEmbed = sp.Attrs["gen_ai.operation.name"] == "embeddings" && sp.Attrs["gen_ai.usage.input_tokens"] == "12"
		case sp.Name == "search memory" && parent.Name == "tools/call recall":
			sawSearch = sp.Attrs["db.system.name"] == "qdrant" && sp.Attrs["db.response.returned_rows"] == "2"
		case sp.Name == "rerank rr" && parent.Name == "tools/call recall":
			sawRerank = true
		case sp.Name == "tools/call fetch_url":
			sawFetchErr = sp.Attrs["error.type"] == "tool_error" && sp.StatusCode == 2 && sp.StatusMessage == ""
		}
	}
	if !sawEmbed || !sawSearch || !sawRerank || !sawFetchErr {
		t.Fatalf("embed %v search %v rerank %v fetch error %v in:\n%+v", sawEmbed, sawSearch, sawRerank, sawFetchErr, spans)
	}
}

func TestUnsampledParentRecordsNothing(t *testing.T) {
	recv := otlptest.New(t)
	exp := otlp.NewExporter(otlp.Config{Endpoint: recv.URL}, "test", "rdnamcp", otlp.Options{})
	defer exp.Shutdown(context.Background())
	tr := otelTracer{exp}
	ctx, sp := tr.Start(context.Background(), "tools/call x", 1, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")
	_, child := tr.Start(ctx, "embeddings", 2, "")
	child.End()
	sp.End()
	exp.ForceFlush(context.Background())
	if n := len(recv.Spans()); n != 0 {
		t.Fatalf("an unsampled caller's trace should record nothing, got %d spans", n)
	}
}
