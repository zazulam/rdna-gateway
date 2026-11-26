package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// modelWorker imitates a llama-server serving some models. Chat requests for
// the classifier model answer with classify(); others echo which model was
// asked for, reporting a file name as "model" as llama-server can.
type modelWorker struct {
	name   string
	models []string
	srv    *httptest.Server

	mu       sync.Mutex
	asked    []string
	classify func() (answer string, delay time.Duration)
}

func newModelWorker(t *testing.T, name string, models ...string) *modelWorker {
	w := &modelWorker{name: name, models: models}
	w.srv = httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *modelWorker) serve(rw http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/health":
		io.WriteString(rw, `{"status":"ok"}`)
	case "/v1/models":
		var ids []string
		for _, m := range w.models {
			ids = append(ids, fmt.Sprintf(`{"id":%q}`, m))
		}
		fmt.Fprintf(rw, `{"data":[%s]}`, strings.Join(ids, ","))
	case "/v1/chat/completions":
		var req struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		w.mu.Lock()
		w.asked = append(w.asked, req.Model)
		cl := w.classify
		w.mu.Unlock()
		if cl != nil && len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "You route chat requests") {
			answer, delay := cl()
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
			fmt.Fprintf(rw, `{"model":"file.gguf","choices":[{"message":{"role":"assistant","content":%q}}]}`, answer)
			return
		}
		content := fmt.Sprintf(`%s answered: \"model\":\"not-this\"`, w.name)
		if !req.Stream {
			rw.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(rw, `{"id":"c1","object":"chat.completion","model":"file.gguf","choices":[{"index":0,"message":{"role":"assistant","content":"%s"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`+"\n", content)
			return
		}
		rw.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(rw, "data: {\"id\":\"c1\",\"model\":\"file.gguf\",\"choices\":[{\"delta\":{\"content\":\"%s\"}}]}\n\n", content)
		io.WriteString(rw, "data: {\"id\":\"c1\",\"model\":\"file.gguf\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(rw, "data: [DONE]\n\n")
	default:
		http.NotFound(rw, r)
	}
}

func (w *modelWorker) lastAsked() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.asked) == 0 {
		return ""
	}
	return w.asked[len(w.asked)-1]
}

// autoLab is three workers serving the default auto targets.
type autoLab struct {
	gemma, gptoss, lfm *modelWorker
	g                  *Gateway
	url                string
}

func newAutoLab(t *testing.T, mod func(*AutoConfig)) *autoLab {
	l := &autoLab{
		gemma:  newModelWorker(t, "gemma", "gemma4:12b"),
		gptoss: newModelWorker(t, "gptoss", "gpt-oss:20b"),
		lfm:    newModelWorker(t, "lfm", "lfm2:8b"),
	}
	l.lfm.classify = func() (string, time.Duration) { return "general", 0 }
	ac := &AutoConfig{}
	if mod != nil {
		mod(ac)
	}
	cfg := &Config{BlockChars: 64, Auto: ac}
	for _, w := range []*modelWorker{l.gemma, l.gptoss, l.lfm} {
		cfg.Workers = append(cfg.Workers, WorkerConfig{Name: w.name, URL: w.srv.URL, Slots: 2})
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	g.logf = t.Logf
	g.checkAllSync(context.Background())
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	l.g, l.url = g, srv.URL
	return l
}

type autoResp struct {
	status                      int
	worker, model, route, wrout string
	body                        string
}

func (l *autoLab) post(t *testing.T, req map[string]any, hdr ...string) autoResp {
	t.Helper()
	if _, ok := req["model"]; !ok {
		req["model"] = "auto"
	}
	b, _ := json.Marshal(req)
	hr, _ := http.NewRequest(http.MethodPost, l.url+"/v1/chat/completions", bytes.NewReader(b))
	for i := 0; i+1 < len(hdr); i += 2 {
		hr.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return autoResp{status: resp.StatusCode, worker: resp.Header.Get("X-Gateway-Worker"),
		model: resp.Header.Get("X-Gateway-Model"), route: resp.Header.Get("X-Gateway-Route"),
		wrout: resp.Header.Get("X-Gateway-Worker-Route"), body: string(out)}
}

func msgs(pairs ...string) []any {
	var out []any
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{"role": pairs[i], "content": pairs[i+1]})
	}
	return out
}

func TestAutoRuleDecisions(t *testing.T) {
	ac := &AutoConfig{}
	if err := ac.normalize(); err != nil {
		t.Fatal(err)
	}
	a := newAutoRouter(ac)
	tool := []any{map[string]any{"type": "function", "function": map[string]any{"name": "read_file"}}}
	cases := []struct {
		name          string
		req           map[string]any
		class, reason string // "" = rules don't decide
	}{
		{"tools offered", map[string]any{"tools": tool, "messages": msgs("user", "hi")}, "code", autoTools},
		{"tool result in history", map[string]any{"messages": append(msgs("user", "list files"),
			map[string]any{"role": "assistant", "tool_calls": []any{}}, map[string]any{"role": "tool", "content": "a.go"})}, "code", autoTools},
		{"tiny max_tokens", map[string]any{"max_tokens": 20.0, "messages": msgs("user", "name this chat")}, "quick", autoShort},
		{"quick hint in system", map[string]any{"messages": msgs("system", "Generate a title for this conversation.", "user", "```go\nfunc main(){}\n```")}, "quick", autoHint},
		{"code hint in system", map[string]any{"messages": msgs("system", "You are a coding assistant.", "user", "why?")}, "code", autoHint},
		{"code fence", map[string]any{"messages": msgs("user", "fix this:\n```py\nprint(1\n```")}, "code", autoCodeFence},
		{"long prompt", map[string]any{"messages": msgs("user", strings.Repeat("words ", 5000))}, "general", autoLong},
		{"ordinary question", map[string]any{"messages": msgs("user", "what's a good soil for tomatoes?")}, "", ""},
		{"large max_tokens", map[string]any{"max_tokens": 2000.0, "messages": msgs("user", "tell me a story")}, "", ""},
	}
	for _, tc := range cases {
		c, r, ok := a.rules(extractFeatures(tc.req))
		if tc.class == "" {
			if ok {
				t.Errorf("%s: rules decided %s/%s, want undecided", tc.name, c, r)
			}
			continue
		}
		if c != tc.class || r != tc.reason {
			t.Errorf("%s: got %s/%s, want %s/%s", tc.name, c, r, tc.class, tc.reason)
		}
	}
}

func TestAutoConfigValidation(t *testing.T) {
	for _, bad := range []AutoConfig{
		{Default: "nope"},
		{Classes: []AutoClass{{Name: "a"}}},
		{Classes: []AutoClass{{Name: "a", Models: []string{"auto"}}}},
		{Classes: []AutoClass{{Name: "a", Models: []string{"m"}}, {Name: "a", Models: []string{"m"}}}},
		{Rules: AutoRules{Tools: "missing"}},
		{Classifier: "auto"},
	} {
		if err := bad.normalize(); err == nil {
			t.Errorf("%+v: want an error", bad)
		}
	}
	// Custom classes without the default names: rules that point at missing
	// classes switch off instead of failing.
	ok := AutoConfig{Classes: []AutoClass{{Name: "big", Models: []string{"m"}}}}
	if err := ok.normalize(); err != nil {
		t.Fatal(err)
	}
	if ok.Default != "big" || ok.Rules.Tools != ruleOff || ok.Rules.Long != ruleOff {
		t.Errorf("defaults: %+v", ok)
	}
}

func TestAutoClassifierPicksClass(t *testing.T) {
	l := newAutoLab(t, nil)
	l.lfm.classify = func() (string, time.Duration) { return "Quick.", 0 }
	r := l.post(t, map[string]any{"stream": true, "messages": msgs("user", "say hi to my sister")})
	if r.status != 200 || r.route != "auto:quick:classifier" || r.worker != "lfm" || r.model != "lfm2:8b" {
		t.Fatalf("got %+v", r)
	}
	if got := l.lfm.lastAsked(); got != "lfm2:8b" {
		t.Errorf("worker was asked for %q, want lfm2:8b", got)
	}
}

func TestAutoClassifierTimeoutFallsBack(t *testing.T) {
	l := newAutoLab(t, func(a *AutoConfig) { a.ClassifierTimeout = Duration(50 * time.Millisecond) })
	l.lfm.classify = func() (string, time.Duration) { return "code", 2 * time.Second }
	start := time.Now()
	r := l.post(t, map[string]any{"messages": msgs("user", "what's a good soil for tomatoes?")})
	if took := time.Since(start); took > time.Second {
		t.Errorf("request took %v: the classifier timeout didn't bound it", took)
	}
	if r.status != 200 || r.route != "auto:general:classifier_timeout" || r.model != "gemma4:12b" {
		t.Fatalf("got %+v", r)
	}
	var m strings.Builder
	l.g.auto.writeMetrics(&m)
	if !strings.Contains(m.String(), `rdnagw_auto_classifier_duration_seconds_count{worker="lfm",model="lfm2:8b",outcome="timeout"} 1`) {
		t.Errorf("classifier timeout not in metrics:\n%s", m.String())
	}
}

func TestAutoClassifierUnsureOrOff(t *testing.T) {
	l := newAutoLab(t, nil)
	l.lfm.classify = func() (string, time.Duration) { return "I think it is about plants", 0 }
	r := l.post(t, map[string]any{"messages": msgs("user", "what's a good soil for tomatoes?")})
	if r.route != "auto:general:classifier_unsure" {
		t.Errorf("unsure: got %+v", r)
	}
	l2 := newAutoLab(t, func(a *AutoConfig) { a.Classifier = "off" })
	r = l2.post(t, map[string]any{"messages": msgs("user", "what's a good soil for tomatoes?")})
	if r.route != "auto:general:classifier_off" || len(l2.lfm.asked) != 0 {
		t.Errorf("off: got %+v, classifier asked %v", r, l2.lfm.asked)
	}
}

func TestAutoUnhealthyTargetFallsBack(t *testing.T) {
	l := newAutoLab(t, nil)
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "read_file"}}}

	// The health check already knows gpt-oss is down: skip straight to gemma4.
	l.gptoss.srv.Close()
	l.g.checkAllSync(context.Background())
	r := l.post(t, map[string]any{"tools": tools, "messages": msgs("user", "list the files")})
	if r.status != 200 || r.route != "auto:code:tools" || r.model != "gemma4:12b" || r.worker != "gemma" {
		t.Fatalf("known-down target: got %+v", r)
	}

	// Still believed healthy, but refuses connections: fail over mid-request.
	l2 := newAutoLab(t, nil)
	l2.gptoss.srv.Close()
	r = l2.post(t, map[string]any{"tools": tools, "messages": msgs("user", "list the files")})
	if r.status != 200 || r.model != "gemma4:12b" || r.worker != "gemma" {
		t.Fatalf("dead-on-connect target: got %+v", r)
	}
	if !strings.Contains(r.body, `"model":"gemma4:12b"`) {
		t.Errorf("response model not rewritten: %s", r.body)
	}

	// Classifier down too: the default class still answers.
	l3 := newAutoLab(t, nil)
	l3.lfm.srv.Close()
	l3.g.checkAllSync(context.Background())
	r = l3.post(t, map[string]any{"messages": msgs("user", "what's a good soil for tomatoes?")})
	if r.status != 200 || r.route != "auto:general:classifier_unavailable" || r.model != "gemma4:12b" {
		t.Fatalf("classifier down: got %+v", r)
	}

	// Nothing serves the class: 503, not a hang.
	for _, w := range []*modelWorker{l3.gemma, l3.gptoss} {
		w.srv.Close()
	}
	l3.g.checkAllSync(context.Background())
	if r = l3.post(t, map[string]any{"tools": tools, "messages": msgs("user", "x")}); r.status != 503 {
		t.Fatalf("all down: got %+v", r)
	}
}

func TestAutoStreamingPassthrough(t *testing.T) {
	l := newAutoLab(t, nil)
	r := l.post(t, map[string]any{"stream": true, "messages": msgs("user", "fix:\n```go\nx := 1\n```")})
	if r.status != 200 || r.route != "auto:code:code_fence" || r.worker != "gptoss" || r.wrout != "least-loaded" {
		t.Fatalf("got %+v", r)
	}
	events := strings.Split(strings.TrimSpace(r.body), "\n\n")
	if len(events) != 3 || events[2] != "data: [DONE]" {
		t.Fatalf("stream changed shape: %q", r.body)
	}
	for _, e := range events[:2] {
		var ch struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(e, "data: ")), &ch); err != nil {
			t.Fatalf("chunk %q: %v", e, err)
		}
		if ch.Model != "gpt-oss:20b" {
			t.Errorf("chunk model %q, want gpt-oss:20b", ch.Model)
		}
	}
	// Content that merely mentions "model" is left alone.
	if !strings.Contains(r.body, `gptoss answered: \"model\":\"not-this\"`) {
		t.Errorf("content was altered: %s", r.body)
	}
}

func TestAutoResponseModelRewrite(t *testing.T) {
	l := newAutoLab(t, nil)
	r := l.post(t, map[string]any{"max_tokens": 16.0, "messages": msgs("user", "title please")})
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(r.body), &out); err != nil {
		t.Fatalf("%v: %s", err, r.body)
	}
	if r.route != "auto:quick:max_tokens" || out.Model != "lfm2:8b" || !strings.Contains(out.Choices[0].Message.Content, `"model":"not-this"`) {
		t.Errorf("got %+v, body %s", r, r.body)
	}

	// A named model passes through untouched.
	r = l.post(t, map[string]any{"model": "gemma4:12b", "messages": msgs("user", "hi")})
	if !strings.Contains(r.body, `"model":"file.gguf"`) || r.route != "least-loaded" || r.model != "gemma4:12b" {
		t.Errorf("named model: got %+v", r)
	}
}

func TestRewriteModel(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`data: {"id":"1","model":"x.gguf","choices":[]}`, `data: {"id":"1","model":"m:1","choices":[]}`},
		{`{"model" : "a\"b","n":1}`, `{"model" : "m:1","n":1}`},
		{`{"content":"say \"model\":\"x\""}`, `{"content":"say \"model\":\"x\""}`},
		{`data: [DONE]`, `data: [DONE]`},
		{`{"model":"m:1"}`, `{"model":"m:1"}`},
	} {
		if got := string(rewriteModel([]byte(tc.in), "m:1")); got != tc.want {
			t.Errorf("rewriteModel(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestAutoConversationAffinity(t *testing.T) {
	l := newAutoLab(t, nil)
	conv := msgs("user", "fix:\n```go\nx := 1\n```")
	r := l.post(t, map[string]any{"messages": conv})
	if r.route != "auto:code:code_fence" || r.model != "gpt-oss:20b" {
		t.Fatalf("turn 1: %+v", r)
	}
	// Turn 2 has no code fence and would go to the classifier (which says
	// general): the conversation stays on its model instead.
	conv = append(conv, msgs("assistant", "done", "user", "thanks, now explain why")...)
	r = l.post(t, map[string]any{"messages": conv})
	if r.route != "auto:code:affinity" || r.model != "gpt-oss:20b" {
		t.Fatalf("turn 2: %+v", r)
	}
	// An explicit session header keys the conversation instead.
	r = l.post(t, map[string]any{"max_tokens": 10.0, "messages": msgs("user", "a")}, "X-Gateway-Session", "s1")
	if r.route != "auto:quick:max_tokens" {
		t.Fatalf("session turn 1: %+v", r)
	}
	r = l.post(t, map[string]any{"messages": msgs("user", "something else entirely")}, "X-Gateway-Session", "s1")
	if r.route != "auto:quick:affinity" || r.model != "lfm2:8b" {
		t.Fatalf("session turn 2: %+v", r)
	}
	// If the pinned model goes away, the conversation is decided afresh.
	l.gptoss.srv.Close()
	l.g.checkAllSync(context.Background())
	conv = append(conv, msgs("assistant", "because", "user", "ok")...)
	if r = l.post(t, map[string]any{"messages": conv}); r.route != "auto:general:classifier" || r.model != "gemma4:12b" {
		t.Fatalf("pinned model down: %+v", r)
	}
}

func TestAutoModelsMetricsAndEndpoints(t *testing.T) {
	l := newAutoLab(t, nil)
	resp, err := http.Get(l.url + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Data []struct {
			ID      string   `json:"id"`
			Workers []string `json:"workers"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	found := false
	for _, m := range list.Data {
		if m.ID == "auto" {
			found = len(m.Workers) == 3
		}
	}
	if !found {
		t.Errorf("/v1/models lacks auto with its workers: %+v", list)
	}

	resp, _ = http.Post(l.url+"/v1/embeddings", "application/json", strings.NewReader(`{"model":"auto","input":"x"}`))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("auto embeddings: status %d, want 400", resp.StatusCode)
	}

	l.post(t, map[string]any{"stream": true, "messages": msgs("user", "fix:\n```go\nx := 1\n```")})
	resp, _ = http.Get(l.url + "/metrics")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		`rdnagw_auto_decisions_total{class="code",reason="code_fence"} 1`,
		`gen_ai_server_request_duration_seconds_count{worker="gptoss",model="gpt-oss:20b",route="auto:code:code_fence",outcome="ok"} 1`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}

func TestExampleConfigEnablesAuto(t *testing.T) {
	c, err := LoadConfig("../../gateway.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if c.Auto == nil || c.Auto.Model != "auto" || len(c.Auto.Classes) != 3 || time.Duration(c.Auto.ClassifierTimeout) != 800*time.Millisecond {
		t.Errorf("auto config: %+v", c.Auto)
	}
}
