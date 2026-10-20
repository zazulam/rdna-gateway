// Package tools implements the agent tools rdnamcp serves: web search through
// SearXNG and a page fetcher that only reaches the public internet.
package tools

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"encoding/json"
)

// SearchResult is one web hit.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"content"`
	Engine  string `json:"engine,omitempty"`
}

// Search queries a SearXNG instance's JSON API.
func Search(ctx context.Context, searxng, query string, n int) ([]SearchResult, error) {
	u := strings.TrimRight(searxng, "/") + "/search?format=json&q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("searxng: %s", resp.Status)
	}
	var r struct {
		Results []SearchResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []SearchResult
	for _, x := range r.Results {
		if seen[x.URL] {
			continue
		}
		seen[x.URL] = true
		x.Snippet = strings.TrimSpace(x.Snippet)
		out = append(out, x)
		if len(out) == n {
			break
		}
	}
	return out, nil
}

// blocked reports addresses a fetch must never reach: this host, the LAN,
// the tailnet (100.64.0.0/10, fd7a:115c:a1e0::/48) and other special ranges.
func blocked(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	for _, c := range specialNets {
		if c.Contains(ip) {
			return true
		}
	}
	return false
}

var specialNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, s := range []string{"100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4", "64:ff9b::/96", "2001:db8::/32"} {
		_, n, _ := net.ParseCIDR(s)
		out = append(out, n)
	}
	return out
}()

var errBlocked = errors.New("refusing to fetch a private, local or tailnet address")

// publicClient checks every address it actually dials (after DNS, on every
// redirect), so neither a hostile hostname nor a redirect reaches inside.
func publicClient(timeout time.Duration) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip == nil || blocked(ip) {
			return errBlocked
		}
		return nil
	}}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           d.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

var (
	reDrop  = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head|nav|footer|form)\b.*?</(script|style|noscript|svg|head|nav|footer|form)>`)
	reBreak = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/h[1-6]|/tr|/pre|/section|/article)\b[^>]*>`)
	reTag   = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpace = regexp.MustCompile(`[ \t\r\f\v]+`)
	reLines = regexp.MustCompile(`\n\s*\n+`)
	reTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// HTMLText reduces a page to readable text.
func HTMLText(page string) string {
	s := reDrop.ReplaceAllString(page, " ")
	s = reBreak.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = reSpace.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.TrimSpace(reLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// Page is a fetched document reduced to text.
type Page struct {
	URL, Title, Text string
	Truncated        bool
}

// Fetch downloads a public http(s) URL and returns its text, at most maxChars.
func Fetch(ctx context.Context, raw string, maxChars int) (*Page, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("not an http(s) URL: %q", raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; rdnamcp/1.0)")
	req.Header.Set("Accept", "text/html,text/plain,application/json;q=0.9,*/*;q=0.5")
	resp, err := publicClient(30 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %s", raw, resp.Status)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/") && !strings.Contains(ct, "json") && !strings.Contains(ct, "xml") && ct != "" {
		return nil, fmt.Errorf("fetch %s: unsupported content type %s", raw, ct)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	p := &Page{URL: resp.Request.URL.String()}
	body := string(b)
	if strings.Contains(ct, "html") || (ct == "" && strings.Contains(strings.ToLower(body[:min(len(body), 512)]), "<html")) {
		if m := reTitle.FindStringSubmatch(body); m != nil {
			p.Title = strings.TrimSpace(html.UnescapeString(m[1]))
		}
		body = HTMLText(body)
	}
	if maxChars > 0 && len(body) > maxChars {
		body, p.Truncated = body[:maxChars], true
	}
	p.Text = body
	return p, nil
}
