// Command rdnabench replays agent-shaped traffic against an OpenAI-compatible
// endpoint and reports latency, throughput and prompt-cache reuse.
//
// Each simulated agent starts from a long system prompt (shared by all agents
// unless -unique-system), then loops: ask, get a reply, append a synthetic tool
// result, ask again — the pattern coding agents produce, where every turn's
// prompt extends the last one.
//
//	rdnabench -url http://localhost:8080 -model gemma4:12b -agents 8 -turns 6
//	rdnabench ... -policy round-robin          # A/B the gateway's routing
//	rdnabench ... -no-cache -unique-system     # every prompt processed from scratch
//	rdnabench ... -json results.jsonl -label x # append a machine-readable record
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

var client = http.DefaultClient

var probeTokens = 160

type result struct {
	kind         string // "turn", "compact", "post-compact", "recall"
	correct      bool   // recall: answered with the planted value
	early        bool   // recall: the fact predates the last compaction
	agent, turn  int
	ttft, total  time.Duration
	promptTokens int
	cachedTokens int
	outputTokens int
	promptTPS    float64 // worker-reported prefill rate for the uncached part
	decodeTPS    float64 // worker-reported per-stream decode rate
	worker       string
	route        string
	err          error
}

// Params are the knobs of one run, echoed into the JSON record.
type Params struct {
	URL          string `json:"url"`
	Model        string `json:"model"`
	Agents       int    `json:"agents"`
	Turns        int    `json:"turns"`
	SystemChars  int    `json:"system_chars"`
	ToolChars    int    `json:"tool_chars"`
	MaxTokens    int    `json:"max_tokens"`
	Policy       string `json:"policy,omitempty"`
	NoCache      bool   `json:"no_cache"`
	UniqueSystem bool   `json:"unique_system"`
	Seed         int64  `json:"seed"`

	CompactAt     int    `json:"compact_at_tokens,omitempty"`
	CompactStyle  string `json:"compact_style,omitempty"`
	CompactTokens int    `json:"compact_tokens,omitempty"`
	CompactKeep   int    `json:"compact_keep,omitempty"`
	Facts         bool   `json:"facts,omitempty"`
}

// Summary is one run's measurements.
type Summary struct {
	Label  string            `json:"label"`
	Tags   map[string]string `json:"tags,omitempty"`
	Params Params            `json:"params"`
	Time   string            `json:"time"`

	OK        int     `json:"ok"`
	Failed    int     `json:"failed"`
	WallS     float64 `json:"wall_s"`
	OutputTok int     `json:"output_tokens"`
	AggTPS    float64 `json:"aggregate_output_tps"`

	TTFTp50       float64 `json:"ttft_p50_ms"`
	TTFTp95       float64 `json:"ttft_p95_ms"`
	FirstTTFTp50  float64 `json:"first_turn_ttft_p50_ms"`
	FollowTTFTp50 float64 `json:"followup_ttft_p50_ms"`
	FollowTTFTp95 float64 `json:"followup_ttft_p95_ms"`
	LatencyP50    float64 `json:"latency_p50_ms"`
	LatencyP95    float64 `json:"latency_p95_ms"`

	PromptTok      int     `json:"prompt_tokens"`
	CachedTok      int     `json:"cached_tokens"`
	CacheReusePct  float64 `json:"cache_reuse_pct"`
	MeanPromptTok  float64 `json:"mean_prompt_tokens"`
	MaxPromptTok   int     `json:"max_prompt_tokens"`
	PrefillTPS     float64 `json:"mean_prefill_tps"`
	StreamDecodeTP float64 `json:"mean_stream_decode_tps"`

	Compactions        int     `json:"compactions,omitempty"`
	CompactMeanMs      float64 `json:"compact_mean_ms,omitempty"`
	PostCompactTTFTp50 float64 `json:"post_compact_ttft_p50_ms,omitempty"`
	PostCompactCache   float64 `json:"post_compact_cache_pct,omitempty"`
	RecallAsked        int     `json:"recall_asked,omitempty"`
	RecallPct          float64 `json:"recall_pct,omitempty"`
	RecallEarlyPct     float64 `json:"recall_compacted_pct,omitempty"`
	RecallLatePct      float64 `json:"recall_recent_pct,omitempty"`

	Needle []NeedleResult `json:"needle,omitempty"`

	Workers map[string]int `json:"workers,omitempty"`
	Routes  map[string]int `json:"routes,omitempty"`
	Errors  []string       `json:"errors,omitempty"`
}

// NeedleResult is one needle-in-a-haystack probe.
type NeedleResult struct {
	TargetTokens int     `json:"target_tokens"`
	PromptTokens int     `json:"prompt_tokens"`
	DepthPct     int     `json:"depth_pct"`
	Correct      bool    `json:"correct"`
	TTFTMs       float64 `json:"ttft_ms"`
	Answer       string  `json:"answer"`
}

func main() {
	var p Params
	flag.StringVar(&p.URL, "url", "http://localhost:8080", "base URL (gateway or a single llama-server)")
	flag.StringVar(&p.Model, "model", "", "model id")
	flag.IntVar(&p.Agents, "agents", 4, "concurrent agents")
	flag.IntVar(&p.Turns, "turns", 5, "turns per agent")
	flag.IntVar(&p.SystemChars, "system-chars", 8000, "system prompt size in bytes (~3 bytes/token for this filler)")
	flag.IntVar(&p.ToolChars, "tool-chars", 2000, "tool-result size appended per turn")
	flag.IntVar(&p.MaxTokens, "max-tokens", 128, "tokens per reply (generation ignores EOS, so every reply is exactly this long)")
	flag.StringVar(&p.Policy, "policy", "", "X-Gateway-Policy header")
	flag.BoolVar(&p.NoCache, "no-cache", false, "send cache_prompt=false: the worker re-processes every prompt in full")
	flag.BoolVar(&p.UniqueSystem, "unique-system", false, "give each agent its own system prompt (no cross-agent sharing)")
	flag.Int64Var(&p.Seed, "seed", 1, "RNG seed for synthetic content")
	flag.IntVar(&p.CompactAt, "compact-at", 0, "compact an agent's history once its prompt passes this many tokens (0 = never)")
	flag.StringVar(&p.CompactStyle, "compact-style", "append", "append: keep the system prompt as a stable prefix and put the summary after it; rewrite: fold the summary into the system prompt")
	flag.IntVar(&p.CompactTokens, "compact-tokens", 512, "max tokens for the summary")
	flag.IntVar(&p.CompactKeep, "compact-keep", 2, "most recent messages kept verbatim after compaction")
	flag.BoolVar(&p.Facts, "facts", false, "plant a key=value fact in every tool result and quiz the agent on some at the end")
	flag.IntVar(&probeTokens, "probe-tokens", 160, "token budget for recall/needle answers (raise for models that always reason)")
	needle := flag.String("needle", "", "needle-in-a-haystack mode: comma-separated context sizes in tokens, e.g. 4000,16000,28000")
	needleDepths := flag.String("needle-depths", "0,25,50,75,100", "needle depths in percent")
	key := flag.String("key", os.Getenv("RDNAGW_API_KEY"), "bearer token")
	timeout := flag.Duration("timeout", 10*time.Minute, "per-request timeout")
	jsonOut := flag.String("json", "", "append a JSON summary line to this file")
	label := flag.String("label", "", "label for the JSON record")
	tagsFlag := flag.String("tags", "", "comma-separated k=v tags for the JSON record")
	flag.Parse()
	// llama-server closes idle keep-alive connections after 5s; retire ours
	// sooner so a request never lands on a socket being closed.
	client = &http.Client{Timeout: *timeout, Transport: &http.Transport{
		MaxIdleConnsPerHost: 64, IdleConnTimeout: 4 * time.Second,
	}}
	if p.Model == "" {
		fmt.Fprintln(os.Stderr, "-model is required")
		os.Exit(2)
	}

	finish := func(s Summary) {
		s.Params, s.Label, s.Time = p, *label, time.Now().UTC().Format(time.RFC3339)
		if *tagsFlag != "" {
			s.Tags = map[string]string{}
			for _, kv := range strings.Split(*tagsFlag, ",") {
				k, v, _ := strings.Cut(kv, "=")
				s.Tags[k] = v
			}
		}
		printSummary(s)
		if *jsonOut != "" {
			f, err := os.OpenFile(*jsonOut, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			json.NewEncoder(f).Encode(s)
			f.Close()
		}
		if s.OK == 0 {
			os.Exit(1)
		}
	}
	if *needle != "" {
		finish(runNeedle(p, *key, ints(*needle), ints(*needleDepths)))
		return
	}

	shared := synth(rand.New(rand.NewSource(p.Seed)), p.SystemChars, "system")
	results := make(chan result, p.Agents*(p.Turns*2+3))
	var wg sync.WaitGroup
	start := time.Now()
	for a := 0; a < p.Agents; a++ {
		wg.Add(1)
		go func(a int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(p.Seed*1000 + int64(a) + 1))
			system := shared
			if p.UniqueSystem {
				system = synth(rand.New(rand.NewSource(p.Seed*1000+int64(a)+500)), p.SystemChars, fmt.Sprintf("system%d", a))
			}
			task := fmt.Sprintf("Agent %d task: refactor module %d and explain each step.", a, a)
			msgs := []map[string]any{
				{"role": "system", "content": system},
				{"role": "user", "content": task},
			}
			var facts []fact
			lastPrompt, postCompact := 0, false
			for t := 0; t < p.Turns; t++ {
				if p.CompactAt > 0 && lastPrompt > p.CompactAt {
					cr, next := compact(p, *key, msgs, system, task)
					cr.agent, cr.turn = a, t
					results <- cr
					if cr.err != nil {
						return
					}
					msgs, postCompact = next, true
					for i := range facts {
						facts[i].compacted = true
					}
				}
				r, reply := turn(p, *key, msgs, opts{maxTokens: p.MaxTokens, ignoreEOS: true, temp: 0.7})
				r.agent, r.turn, r.kind = a, t, "turn"
				if postCompact {
					r.kind, postCompact = "post-compact", false
				}
				results <- r
				if r.err != nil {
					return
				}
				lastPrompt = r.promptTokens
				tool := synth(rng, p.ToolChars, "tool")
				if p.Facts {
					f := fact{key: fmt.Sprintf("%s-%d", strings.ToUpper(words[rng.Intn(len(words))]), rng.Intn(9000)+1000),
						val: fmt.Sprint(rng.Intn(900000) + 100000)}
					facts = append(facts, f)
					tool = "FACT: the value of key " + f.key + " is " + f.val + ".\n" + tool
				}
				msgs = append(msgs,
					map[string]any{"role": "assistant", "content": reply},
					map[string]any{"role": "user", "content": "Tool result:\n" + tool + "\nContinue."},
				)
			}
			for _, f := range probeSet(facts) {
				q := append(append([]map[string]any(nil), msgs...), map[string]any{"role": "user",
					"content": "Quick check before you continue: what is the value of key " + f.key + "? Answer with just the number."})
				r, ans := turn(p, *key, q, opts{maxTokens: probeTokens, temp: 0, noThink: true})
				r.agent, r.kind, r.early = a, "recall", f.compacted
				r.correct = strings.Contains(ans, f.val)
				results <- r
			}
		}(a)
	}
	wg.Wait()
	close(results)
	finish(summarize(results, time.Since(start)))
}

type fact struct {
	key, val  string
	compacted bool // planted before the agent's latest compaction
}

// probeSet picks the facts to quiz: oldest, middle and newest.
func probeSet(fs []fact) []fact {
	if len(fs) <= 3 {
		return fs
	}
	return []fact{fs[0], fs[len(fs)/2], fs[len(fs)-1]}
}

type opts struct {
	maxTokens int
	ignoreEOS bool
	temp      float64
	noThink   bool // quality probes: ask the template to skip reasoning
}

// compact asks the model to summarize the history (as Hive's compaction does),
// then rebuilds the conversation around the summary.
func compact(p Params, key string, msgs []map[string]any, system, task string) (result, []map[string]any) {
	q := append(append([]map[string]any(nil), msgs...), map[string]any{"role": "user", "content": "Compact this conversation: " +
		"write a dense summary of the work so far that will replace the history. Copy every FACT line verbatim."})
	r, summary := turn(p, key, q, opts{maxTokens: p.CompactTokens, temp: 0, noThink: true})
	r.kind = "compact"
	keep := p.CompactKeep
	if keep > len(msgs)-2 {
		keep = len(msgs) - 2
	}
	recent := msgs[len(msgs)-keep:]
	var next []map[string]any
	if p.CompactStyle == "rewrite" {
		next = []map[string]any{
			{"role": "system", "content": "Session summary:\n" + summary + "\n\n" + system},
			{"role": "user", "content": task},
		}
	} else {
		next = []map[string]any{
			{"role": "system", "content": system},
			{"role": "user", "content": task + "\n\nSummary of the session so far:\n" + summary},
		}
	}
	return r, append(next, recent...)
}

func ints(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ",") {
		var v int
		fmt.Sscan(strings.TrimSpace(f), &v)
		out = append(out, v)
	}
	return out
}

// runNeedle hides one fact at each depth of filler contexts of each size and
// asks for it back, cache off so every probe is a fresh prompt.
func runNeedle(p Params, key string, sizes, depths []int) Summary {
	rng := rand.New(rand.NewSource(p.Seed))
	s := Summary{}
	start := time.Now()
	for _, size := range sizes {
		for _, depth := range depths {
			chars := size * 4 // the filler runs ~4 bytes/token
			hay := synth(rng, chars, "log")
			code := fmt.Sprint(rng.Intn(900000) + 100000)
			proj := strings.ToUpper(words[rng.Intn(len(words))])
			needle := "\nIMPORTANT: the access code for project " + proj + " is " + code + ".\n"
			at := len(hay) * depth / 100
			if i := strings.LastIndexByte(hay[:at], '\n'); i >= 0 {
				at = i
			}
			msgs := []map[string]any{
				{"role": "system", "content": "You answer questions about the document the user provides."},
				{"role": "user", "content": "Document:\n" + hay[:at] + needle + hay[at:] +
					"\n\nWhat is the access code for project " + proj + "? Answer with just the number."},
			}
			pp := p
			pp.NoCache = true
			r, ans := turn(pp, key, msgs, opts{maxTokens: probeTokens, temp: 0, noThink: true})
			if r.err != nil {
				s.Failed++
				s.Errors = append(s.Errors, r.err.Error())
				continue
			}
			s.OK++
			ok := strings.Contains(ans, code)
			s.Needle = append(s.Needle, NeedleResult{TargetTokens: size, PromptTokens: r.promptTokens, DepthPct: depth,
				Correct: ok, TTFTMs: float64(r.ttft.Microseconds()) / 1000, Answer: strings.TrimSpace(ans)})
			if ok {
				s.RecallAsked++
			}
		}
	}
	s.WallS = time.Since(start).Seconds()
	if n := len(s.Needle); n > 0 {
		s.RecallPct = 100 * float64(s.RecallAsked) / float64(n)
		s.RecallAsked = n
	}
	return s
}

func turn(p Params, key string, msgs []map[string]any, o opts) (result, string) {
	req := map[string]any{
		"model": p.Model, "messages": msgs, "max_tokens": o.maxTokens, "stream": true,
		"stream_options": map[string]any{"include_usage": true},
		"temperature":    o.temp,
	}
	if o.ignoreEOS {
		req["ignore_eos"] = true // fixed-length replies keep runs comparable
	}
	if o.noThink {
		// enable_thinking: Gemma 4 / Qwen 3 templates; reasoning_effort: gpt-oss.
		req["chat_template_kwargs"] = map[string]any{"enable_thinking": false, "reasoning_effort": "low"}
	}
	if p.NoCache {
		req["cache_prompt"] = false
	}
	body, _ := json.Marshal(req)
	hreq, _ := http.NewRequest(http.MethodPost, strings.TrimRight(p.URL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	hreq.Header = hreqHeaders(key, p.Policy)
	t0 := time.Now()
	resp, err := client.Do(hreq)
	if err != nil && staleConn(err) {
		// Lost the keep-alive race anyway: nothing was processed, resend once.
		hreq, _ = http.NewRequest(http.MethodPost, hreq.URL.String(), bytes.NewReader(body))
		hreq.Header = hreqHeaders(key, p.Policy)
		t0 = time.Now()
		resp, err = client.Do(hreq)
	}
	if err != nil {
		return result{err: err}, ""
	}
	defer resp.Body.Close()
	r := result{worker: resp.Header.Get("X-Gateway-Worker"), route: resp.Header.Get("X-Gateway-Route")}
	if resp.StatusCode != 200 {
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		r.err = fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(b.String()))
		return r, ""
	}
	var reply strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "data: ")
		if line == "" || line == "[DONE]" || !strings.HasPrefix(line, "{") {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens        int `json:"prompt_tokens"`
				CompletionTokens    int `json:"completion_tokens"`
				PromptTokensDetails struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
			Timings *struct {
				CacheN    int     `json:"cache_n"`
				PromptPS  float64 `json:"prompt_per_second"`
				PredictPS float64 `json:"predicted_per_second"`
			} `json:"timings"`
		}
		if json.Unmarshal([]byte(line), &c) != nil {
			continue
		}
		for _, ch := range c.Choices {
			if (ch.Delta.Content != "" || ch.Delta.ReasoningContent != "") && r.ttft == 0 {
				r.ttft = time.Since(t0)
			}
			reply.WriteString(ch.Delta.Content)
		}
		if c.Usage != nil {
			r.promptTokens = c.Usage.PromptTokens
			r.outputTokens = c.Usage.CompletionTokens
			r.cachedTokens = c.Usage.PromptTokensDetails.CachedTokens
		}
		if c.Timings != nil {
			if c.Timings.CacheN > r.cachedTokens {
				r.cachedTokens = c.Timings.CacheN
			}
			r.promptTPS, r.decodeTPS = c.Timings.PromptPS, c.Timings.PredictPS
		}
	}
	r.total = time.Since(t0)
	r.err = sc.Err()
	if reply.Len() == 0 {
		reply.WriteString("(no content)")
	}
	return r, reply.String()
}

// staleConn reports a request that died on a keep-alive socket the server was
// closing, before any response: safe to resend.
func staleConn(err error) bool {
	m := err.Error()
	return strings.Contains(m, "server closed idle connection") || strings.HasSuffix(m, ": EOF") ||
		strings.Contains(m, "connection reset by peer") || strings.Contains(m, "use of closed network connection")
}

func hreqHeaders(key, policy string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
	if policy != "" {
		h.Set("X-Gateway-Policy", policy)
	}
	return h
}

func summarize(ch <-chan result, wall time.Duration) Summary {
	s := Summary{WallS: wall.Seconds(), Workers: map[string]int{}, Routes: map[string]int{}}
	var ttft, first, follow, lat []float64
	var prefill, decode []float64
	var compactMs, postTTFT, postCache []float64
	var recallOK, earlyN, earlyOK, lateN, lateOK int
	for r := range ch {
		if r.err != nil {
			s.Failed++
			if len(s.Errors) < 5 {
				s.Errors = append(s.Errors, r.err.Error())
			}
			continue
		}
		switch r.kind {
		case "recall":
			s.RecallAsked++
			if r.correct {
				recallOK++
			}
			if r.early {
				earlyN++
				if r.correct {
					earlyOK++
				}
			} else {
				lateN++
				if r.correct {
					lateOK++
				}
			}
			continue
		case "compact":
			s.Compactions++
			compactMs = append(compactMs, float64(r.total.Microseconds())/1000)
			continue
		case "post-compact":
			postTTFT = append(postTTFT, float64(r.ttft.Microseconds())/1000)
			if r.promptTokens > 0 {
				postCache = append(postCache, 100*float64(r.cachedTokens)/float64(r.promptTokens))
			}
		}
		s.OK++
		ms := float64(r.ttft.Microseconds()) / 1000
		ttft = append(ttft, ms)
		if r.turn == 0 {
			first = append(first, ms)
		} else {
			follow = append(follow, ms)
		}
		lat = append(lat, float64(r.total.Microseconds())/1000)
		s.PromptTok += r.promptTokens
		s.CachedTok += r.cachedTokens
		s.OutputTok += r.outputTokens
		if r.promptTokens > s.MaxPromptTok {
			s.MaxPromptTok = r.promptTokens
		}
		if r.promptTPS > 0 && r.promptTokens-r.cachedTokens >= 64 {
			prefill = append(prefill, r.promptTPS) // tiny prefills give noisy rates
		}
		if r.decodeTPS > 0 {
			decode = append(decode, r.decodeTPS)
		}
		if r.worker != "" {
			s.Workers[r.worker]++
		}
		if r.route != "" {
			s.Routes[r.route]++
		}
	}
	if s.OK == 0 {
		return s
	}
	s.AggTPS = float64(s.OutputTok) / s.WallS
	s.TTFTp50, s.TTFTp95 = pct(ttft, 50), pct(ttft, 95)
	s.FirstTTFTp50 = pct(first, 50)
	s.FollowTTFTp50, s.FollowTTFTp95 = pct(follow, 50), pct(follow, 95)
	s.LatencyP50, s.LatencyP95 = pct(lat, 50), pct(lat, 95)
	if s.PromptTok > 0 {
		s.CacheReusePct = 100 * float64(s.CachedTok) / float64(s.PromptTok)
	}
	s.MeanPromptTok = float64(s.PromptTok) / float64(s.OK)
	s.PrefillTPS, s.StreamDecodeTP = mean(prefill), mean(decode)
	s.CompactMeanMs, s.PostCompactTTFTp50, s.PostCompactCache = mean(compactMs), pct(postTTFT, 50), mean(postCache)
	pctOf := func(a, b int) float64 {
		if b == 0 {
			return 0
		}
		return 100 * float64(a) / float64(b)
	}
	s.RecallPct, s.RecallEarlyPct, s.RecallLatePct = pctOf(recallOK, s.RecallAsked), pctOf(earlyOK, earlyN), pctOf(lateOK, lateN)
	return s
}

func printSummary(s Summary) {
	if s.Label != "" {
		fmt.Printf("== %s\n", s.Label)
	}
	fmt.Printf("requests      %d ok, %d failed in %.1fs\n", s.OK, s.Failed, s.WallS)
	if s.OK == 0 {
		for _, e := range s.Errors {
			fmt.Println("  error:", e)
		}
		return
	}
	fmt.Printf("TTFT ms       p50 %.0f  p95 %.0f  (first turn p50 %.0f; follow-ups p50 %.0f  p95 %.0f)\n",
		s.TTFTp50, s.TTFTp95, s.FirstTTFTp50, s.FollowTTFTp50, s.FollowTTFTp95)
	fmt.Printf("output        %d tok, %.1f tok/s aggregate, %.1f tok/s per stream\n", s.OutputTok, s.AggTPS, s.StreamDecodeTP)
	fmt.Printf("prompt        %d tok (mean %.0f, max %d), %.1f%% from cache, prefill %.0f tok/s\n",
		s.PromptTok, s.MeanPromptTok, s.MaxPromptTok, s.CacheReusePct, s.PrefillTPS)
	if s.Compactions > 0 {
		fmt.Printf("compaction    %d done, %.1fs each; next-turn TTFT p50 %.0f ms, %.0f%% cached\n",
			s.Compactions, s.CompactMeanMs/1000, s.PostCompactTTFTp50, s.PostCompactCache)
	}
	if s.RecallAsked > 0 && len(s.Needle) == 0 {
		fmt.Printf("recall        %.0f%% of %d (compacted-away facts %.0f%%, recent %.0f%%)\n",
			s.RecallPct, s.RecallAsked, s.RecallEarlyPct, s.RecallLatePct)
	}
	for _, n := range s.Needle {
		fmt.Printf("needle        %6d tok @%3d%%  %-5v %6.0f ms  %q\n", n.PromptTokens, n.DepthPct, n.Correct, n.TTFTMs, n.Answer)
	}
	if len(s.Workers) > 0 {
		fmt.Printf("workers       %v\n", s.Workers)
		fmt.Printf("routes        %v\n", s.Routes)
	}
	for _, e := range s.Errors {
		fmt.Println("  error:", e)
	}
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[int(float64(len(s)-1)*p/100)]
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	t := 0.0
	for _, x := range xs {
		t += x
	}
	return t / float64(len(xs))
}

// synth makes deterministic, token-diverse filler text (code-ish lines) of
// about n bytes.
var words = []string{"func", "return", "error", "context", "worker", "buffer", "index", "cache",
	"request", "stream", "token", "slot", "prefix", "router", "health", "config", "model",
	"session", "agent", "tool", "result", "file", "path", "line", "value", "struct"}

func synth(rng *rand.Rand, n int, kind string) string {
	var b strings.Builder
	for line := 0; b.Len() < n; line++ {
		fmt.Fprintf(&b, "%s %d:", kind, line)
		for i := 0; i < 8; i++ {
			b.WriteByte(' ')
			b.WriteString(words[rng.Intn(len(words))])
		}
		b.WriteByte('\n')
	}
	return b.String()
}
