package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// AutoConfig is the "auto" virtual model: a request naming it is sorted into
// a class, by rules when they can tell and by a small classifier model when
// they can't, and sent to the first healthy model of that class.
type AutoConfig struct {
	// Model is the name clients ask for (default "auto").
	Model string `json:"model,omitempty"`
	// Classifier is the model, served by this gateway's own workers, that
	// sorts requests the rules can't (default "lfm2:8b"). "off" skips it, so
	// undecided requests go to the default class.
	Classifier string `json:"classifier,omitempty"`
	// ClassifierTimeout bounds the classifier call (default "800ms"). On
	// timeout or error the request goes to the default class.
	ClassifierTimeout Duration `json:"classifier_timeout,omitempty"`
	// Default is the class for undecided requests (default "general").
	Default string `json:"default,omitempty"`
	// Classes in order of preference for ties. Each lists its target models
	// in order: the first with a healthy worker answers.
	Classes []AutoClass `json:"classes,omitempty"`
	Rules   AutoRules   `json:"rules,omitempty"`
	// AffinityTTL is how long a conversation keeps its first decision
	// (default "30m"), so its later turns stay on the model whose worker
	// holds their prefix in cache.
	AffinityTTL Duration `json:"affinity_ttl,omitempty"`
}

// AutoClass is one route class.
type AutoClass struct {
	Name string `json:"name"`
	// Description tells the classifier what belongs here.
	Description string `json:"description,omitempty"`
	// Models to try, in order.
	Models []string `json:"models"`
	// Hints are case-insensitive phrases that, found in the system prompt,
	// put a request in this class without asking the classifier.
	Hints []string `json:"hints,omitempty"`
}

// AutoRules names the class each cheap rule routes to. "off" turns a rule
// off; an empty value takes the default when that class exists.
type AutoRules struct {
	// Tools: the request offers tools or carries tool calls / results (default "code").
	Tools string `json:"tools,omitempty"`
	// Short: max_tokens is at most ShortMaxTokens (default "quick", 64).
	Short          string `json:"short,omitempty"`
	ShortMaxTokens int    `json:"short_max_tokens,omitempty"`
	// CodeFence: the latest user message contains a ``` block (default "code").
	CodeFence string `json:"code_fence,omitempty"`
	// Long: the prompt is at least LongChars bytes (default "general", 24000).
	Long      string `json:"long,omitempty"`
	LongChars int    `json:"long_chars,omitempty"`
}

const ruleOff = "off"

func defaultAutoClasses() []AutoClass {
	return []AutoClass{
		{
			Name:        "code",
			Description: "programming, debugging, code review, shell commands, tool calling and agent steps",
			Models:      []string{"gpt-oss:20b", "gemma4:12b"},
			Hints: []string{"coding assistant", "coding agent", "software engineer", "programming assistant",
				"code review", "unified diff", "you can call tools"},
		},
		{
			Name:        "general",
			Description: "explanations, writing, analysis, advice, long-form answers and ordinary conversation",
			Models:      []string{"gemma4:12b", "gpt-oss:20b"},
		},
		{
			Name:        "quick",
			Description: "very short jobs: a title, a label, a yes/no, a one-line answer, classification or extraction",
			Models:      []string{"lfm2:8b", "gemma4:12b"},
			Hints: []string{"generate a title", "short title", "a title for", "title for this conversation",
				"in one line", "one-line", "one-liner", "one word", "single word", "classify"},
		},
	}
}

func (a *AutoConfig) normalize() error {
	if a.Model == "" {
		a.Model = "auto"
	}
	if a.Classifier == "" {
		a.Classifier = "lfm2:8b"
	}
	if a.ClassifierTimeout <= 0 {
		a.ClassifierTimeout = Duration(800 * time.Millisecond)
	}
	if a.AffinityTTL <= 0 {
		a.AffinityTTL = Duration(30 * time.Minute)
	}
	if len(a.Classes) == 0 {
		a.Classes = defaultAutoClasses()
	}
	names := map[string]bool{}
	for i, c := range a.Classes {
		if c.Name == "" || strings.ContainsAny(c.Name, ": ") {
			return fmt.Errorf("config: auto class %d needs a name without spaces or colons", i)
		}
		if names[c.Name] {
			return fmt.Errorf("config: duplicate auto class %q", c.Name)
		}
		names[c.Name] = true
		if len(c.Models) == 0 {
			return fmt.Errorf("config: auto class %q lists no models", c.Name)
		}
		for _, m := range c.Models {
			if m == a.Model {
				return fmt.Errorf("config: auto class %q targets %q itself", c.Name, a.Model)
			}
		}
	}
	if a.Classifier == a.Model {
		return fmt.Errorf("config: auto classifier can't be %q itself", a.Model)
	}
	if a.Default == "" {
		a.Default = "general"
		if !names[a.Default] {
			a.Default = a.Classes[0].Name
		}
	}
	if !names[a.Default] {
		return fmt.Errorf("config: auto default class %q isn't defined", a.Default)
	}
	r := &a.Rules
	for _, rule := range []struct {
		name string
		v    *string
		def  string
	}{{"tools", &r.Tools, "code"}, {"short", &r.Short, "quick"}, {"code_fence", &r.CodeFence, "code"}, {"long", &r.Long, "general"}} {
		switch {
		case *rule.v == "":
			*rule.v = ruleOff
			if names[rule.def] {
				*rule.v = rule.def
			}
		case *rule.v != ruleOff && !names[*rule.v]:
			return fmt.Errorf("config: auto rule %s names undefined class %q", rule.name, *rule.v)
		}
	}
	if r.ShortMaxTokens <= 0 {
		r.ShortMaxTokens = 64
	}
	if r.LongChars <= 0 {
		r.LongChars = 24000
	}
	return nil
}

// Reasons for an auto decision, as the last part of X-Gateway-Route
// ("auto:<class>:<reason>") and the route metric label.
const (
	autoAffinity    = "affinity"
	autoTools       = "tools"
	autoShort       = "max_tokens"
	autoHint        = "hint"
	autoCodeFence   = "code_fence"
	autoLong        = "long"
	autoClassifier  = "classifier"
	autoTimeout     = "classifier_timeout"
	autoError       = "classifier_error"
	autoUnsure      = "classifier_unsure"
	autoUnavailable = "classifier_unavailable"
	autoOff         = "classifier_off"
)

// autoDecision is where an auto request goes.
type autoDecision struct {
	class, reason string
	models        []string // targets in order; the first had a healthy worker
	key           uint64   // conversation key, 0 = none
}

func (d *autoDecision) label() string { return "auto:" + d.class + ":" + d.reason }

// autoRouter holds the auto model's config and conversation affinity.
type autoRouter struct {
	cfg     *AutoConfig
	classes map[string]*AutoClass

	mu       sync.Mutex
	sessions map[uint64]affinity
	decided  map[[2]string]int64 // (class, reason) → count, for /metrics

	classifierLat *histogram
}

type affinity struct {
	class, model string
	until        time.Time
}

const maxSessions = 20_000

func newAutoRouter(cfg *AutoConfig) *autoRouter {
	a := &autoRouter{cfg: cfg, classes: map[string]*AutoClass{}, sessions: map[uint64]affinity{},
		decided: map[[2]string]int64{},
		classifierLat: newHistogram("rdnagw_auto_classifier_duration_seconds",
			"The auto model's classifier call, by outcome (ok, timeout, error, unsure).", classifierBuckets)}
	a.classifierLat.labels = func(k histKey) string {
		return fmt.Sprintf("worker=%q,model=%q,outcome=%q", k.worker, k.model, k.outcome)
	}
	for i := range cfg.Classes {
		a.classes[cfg.Classes[i].Name] = &cfg.Classes[i]
	}
	return a
}

var classifierBuckets = []float64{0.05, 0.1, 0.2, 0.3, 0.4, 0.6, 0.8, 1, 2, 5}

// features are the parts of a request the rules and classifier look at.
// Nothing here is logged or exported.
type features struct {
	tools     bool
	maxTokens int // 0 = not set
	system    string
	firstUser string
	lastUser  string
	chars     int
	turns     int // user messages
}

func extractFeatures(req map[string]any) features {
	var f features
	if t, ok := req["tools"].([]any); ok && len(t) > 0 {
		f.tools = true
	}
	if t, ok := req["functions"].([]any); ok && len(t) > 0 {
		f.tools = true
	}
	for _, k := range []string{"max_tokens", "max_completion_tokens", "n_predict"} {
		if v, ok := req[k].(float64); ok && v > 0 {
			f.maxTokens = int(v)
			break
		}
	}
	var sys strings.Builder
	if msgs, ok := req["messages"].([]any); ok {
		for _, raw := range msgs {
			m, _ := raw.(map[string]any)
			role, _ := m["role"].(string)
			var b strings.Builder
			writeContent(&b, m["content"])
			text := b.String()
			f.chars += len(text)
			if _, ok := m["tool_calls"]; ok || role == "tool" || role == "function" {
				f.tools = true
			}
			switch role {
			case "system", "developer":
				if f.turns == 0 {
					sys.WriteString(text)
					sys.WriteByte('\n')
				}
			case "user":
				if f.turns == 0 {
					f.firstUser = text
				}
				f.lastUser = text
				f.turns++
			}
		}
	}
	switch p := req["prompt"].(type) {
	case string:
		f.lastUser, f.chars = p, len(p)
	case []any:
		if len(p) > 0 {
			f.lastUser, _ = p[0].(string)
			f.chars = len(f.lastUser)
		}
	}
	f.system = sys.String()
	return f
}

// rules decides a request's class when a cheap signal is clear.
func (a *autoRouter) rules(f features) (class, reason string, ok bool) {
	r := a.cfg.Rules
	if f.tools && r.Tools != ruleOff {
		return r.Tools, autoTools, true
	}
	if f.maxTokens > 0 && f.maxTokens <= r.ShortMaxTokens && r.Short != ruleOff {
		return r.Short, autoShort, true
	}
	if sys := strings.ToLower(f.system); sys != "" {
		for _, c := range a.cfg.Classes {
			for _, h := range c.Hints {
				if h != "" && strings.Contains(sys, strings.ToLower(h)) {
					return c.Name, autoHint, true
				}
			}
		}
	}
	if r.CodeFence != ruleOff && strings.Contains(f.lastUser, "```") {
		return r.CodeFence, autoCodeFence, true
	}
	if r.Long != ruleOff && f.chars >= r.LongChars {
		return r.Long, autoLong, true
	}
	return "", "", false
}

// conversationKey identifies a conversation across turns: an explicit
// X-Gateway-Session header, or else its system prompt and first user message,
// which every later turn repeats. 0 means no key.
func conversationKey(h http.Header, f features) uint64 {
	s := h.Get("X-Gateway-Session")
	if s == "" && f.firstUser == "" {
		return 0
	}
	hs := fnv.New64a()
	if s != "" {
		io.WriteString(hs, "session\x00"+s)
	} else {
		io.WriteString(hs, f.system)
		hs.Write([]byte{0})
		io.WriteString(hs, f.firstUser)
	}
	if k := hs.Sum64(); k != 0 {
		return k
	}
	return 1
}

func (a *autoRouter) lookup(key uint64, now time.Time) (affinity, bool) {
	if key == 0 {
		return affinity{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.sessions[key]
	if !ok || now.After(e.until) {
		return affinity{}, false
	}
	return e, true
}

// remember pins a conversation to the model that answered it.
func (a *autoRouter) remember(key uint64, class, model string, now time.Time) {
	if key == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.sessions) >= maxSessions {
		for k, e := range a.sessions {
			if now.After(e.until) {
				delete(a.sessions, k)
			}
		}
		for k := range a.sessions { // still full: drop arbitrary entries
			if len(a.sessions) < maxSessions*3/4 {
				break
			}
			delete(a.sessions, k)
		}
	}
	a.sessions[key] = affinity{class: class, model: model, until: now.Add(time.Duration(a.cfg.AffinityTTL))}
}

func (a *autoRouter) count(d *autoDecision) {
	a.mu.Lock()
	a.decided[[2]string{d.class, d.reason}]++
	a.mu.Unlock()
}

// targets orders the models to try for class: its own list, then the default
// class's, keeping only models some healthy worker serves, primary workers
// before fallback ones.
func (g *Gateway) targets(class string) []string {
	a := g.auto
	var list []string
	seen := map[string]bool{}
	for _, c := range []string{class, a.cfg.Default} {
		for _, m := range a.classes[c].Models {
			if !seen[m] {
				seen[m] = true
				list = append(list, m)
			}
		}
	}
	var primary, fallback []string
	for _, m := range list {
		switch p, anyUp := g.router.available(m); {
		case p:
			primary = append(primary, m)
		case anyUp:
			fallback = append(fallback, m)
		}
	}
	return append(primary, fallback...)
}

// resolve decides where an auto request goes.
func (g *Gateway) resolveAuto(ctx context.Context, h http.Header, req map[string]any) *autoDecision {
	a := g.auto
	f := extractFeatures(req)
	d := &autoDecision{key: conversationKey(h, f)}
	if e, ok := a.lookup(d.key, time.Now()); ok {
		if p, anyUp := g.router.available(e.model); p || anyUp {
			d.class, d.reason = e.class, autoAffinity
			d.models = []string{e.model}
			for _, m := range g.targets(e.class) {
				if m != e.model {
					d.models = append(d.models, m)
				}
			}
			a.count(d)
			return d
		}
	}
	if c, r, ok := a.rules(f); ok {
		d.class, d.reason = c, r
	} else {
		d.class, d.reason = g.classify(ctx, f)
	}
	d.models = g.targets(d.class)
	a.count(d)
	return d
}

// classifierPrompt is byte-stable, so the classifier's worker keeps it cached.
func (a *autoRouter) classifierPrompt() string {
	var b strings.Builder
	b.WriteString("You route chat requests to the right model. Read the request and reply with one word: the name of its class.\n\nClasses:\n")
	for _, c := range a.cfg.Classes {
		fmt.Fprintf(&b, "- %s: %s\n", c.Name, c.Description)
	}
	b.WriteString("\nReply with the class name only.")
	return b.String()
}

const classifierExcerpt = 1500

// classify asks the classifier model for a class, falling back to the
// default class on timeout, error or an answer it can't read.
func (g *Gateway) classify(ctx context.Context, f features) (class, reason string) {
	a := g.auto
	def := a.cfg.Default
	if a.cfg.Classifier == ruleOff {
		return def, autoOff
	}
	d, err := g.router.Pick(PolicyLeastLoaded, a.cfg.Classifier, nil, 0, nil)
	if err != nil {
		return def, autoUnavailable
	}
	w := d.Worker
	excerpt := f.lastUser
	if len(excerpt) > classifierExcerpt {
		excerpt = excerpt[:classifierExcerpt]
	}
	if f.turns > 1 {
		excerpt = fmt.Sprintf("(turn %d of a conversation)\n%s", f.turns, excerpt)
	}
	body, _ := json.Marshal(map[string]any{
		"model": a.cfg.Classifier,
		"messages": []map[string]string{
			{"role": "system", "content": a.classifierPrompt()},
			{"role": "user", "content": excerpt},
		},
		"max_tokens": 8, "temperature": 0, "stream": false,
	})
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(a.cfg.ClassifierTimeout))
	defer cancel()
	w.inflight.Add(1)
	answer, err := g.complete(ctx, w, body)
	w.inflight.Add(-1)
	outcome := "ok"
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		class, reason, outcome = def, autoTimeout, "timeout"
	case err != nil:
		class, reason, outcome = def, autoError, "error"
	default:
		class, reason = a.parseClass(answer), autoClassifier
		if class == "" {
			class, reason, outcome = def, autoUnsure, "unsure"
		}
	}
	a.classifierLat.observe(histKey{worker: w.Name, model: a.cfg.Classifier, outcome: outcome}, time.Since(start).Seconds())
	return class, reason
}

// complete sends a non-streamed chat completion to w and returns its text.
func (g *Gateway) complete(ctx context.Context, w *Worker, body []byte) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, w.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("classifier: %s", resp.Status)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", errors.New("classifier: no choices")
	}
	return out.Choices[0].Message.Content, nil
}

// parseClass finds the class name the answer mentions first, as a whole word.
func (a *autoRouter) parseClass(answer string) string {
	s := strings.ToLower(answer)
	best, at := "", len(s)+1
	for _, c := range a.cfg.Classes {
		name := strings.ToLower(c.Name)
		for i := 0; i+len(name) <= len(s); {
			j := strings.Index(s[i:], name)
			if j < 0 {
				break
			}
			j += i
			end := j + len(name)
			if (j == 0 || !isWordByte(s[j-1])) && (end == len(s) || !isWordByte(s[end])) {
				if j < at {
					best, at = c.Name, j
				}
				break
			}
			i = j + 1
		}
	}
	return best
}

func isWordByte(b byte) bool {
	return b == '_' || b == '-' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z'
}

// withModel returns body with its "model" field set to model. Other fields
// pass through byte for byte.
func withModel(body []byte, model string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(model)
	m["model"] = b
	return json.Marshal(m)
}

var modelKey = []byte(`"model"`)

// rewriteModel sets the top-level "model" value in one JSON body or SSE data
// line to model, so an auto response names the model that answered. Keys
// inside string values are escaped (\"model\"), so they never match.
func rewriteModel(line []byte, model string) []byte {
	i := bytes.Index(line, modelKey)
	if i < 0 {
		return line
	}
	j := i + len(modelKey)
	for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
		j++
	}
	if j >= len(line) || line[j] != ':' {
		return line
	}
	j++
	for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
		j++
	}
	if j >= len(line) || line[j] != '"' {
		return line
	}
	end := j + 1
	for end < len(line) && line[end] != '"' {
		if line[end] == '\\' {
			end++
		}
		end++
	}
	if end >= len(line) {
		return line
	}
	quoted, _ := json.Marshal(model)
	if bytes.Equal(line[j:end+1], quoted) {
		return line
	}
	out := make([]byte, 0, len(line)+len(quoted))
	out = append(out, line[:j]...)
	out = append(out, quoted...)
	return append(out, line[end+1:]...)
}

// writeMetrics renders the auto model's decision counts and classifier latency.
func (a *autoRouter) writeMetrics(b *strings.Builder) {
	a.mu.Lock()
	keys := make([][2]string, 0, len(a.decided))
	for k := range a.decided {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	b.WriteString("# HELP rdnagw_auto_decisions_total Requests for the auto model, by chosen class and why.\n# TYPE rdnagw_auto_decisions_total counter\n")
	for _, k := range keys {
		fmt.Fprintf(b, "rdnagw_auto_decisions_total{class=%q,reason=%q} %d\n", k[0], k[1], a.decided[k])
	}
	a.mu.Unlock()
	a.classifierLat.write(b)
}
