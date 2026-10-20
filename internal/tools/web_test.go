package tools

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBlockedRanges(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.5", "172.16.0.1", "100.64.0.20", "169.254.169.254",
		"0.0.0.0", "::1", "fd7a:115c:a1e0::1", "fe80::1", "::ffff:127.0.0.1"} {
		if !blocked(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "140.82.112.3", "2606:4700:4700::1111"} {
		if blocked(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestFetchRefusesLocalAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte("secret")) }))
	defer srv.Close()
	_, err := Fetch(context.Background(), srv.URL, 100)
	if !errors.Is(err, errBlocked) {
		t.Fatalf("want errBlocked, got %v", err)
	}
	if _, err := Fetch(context.Background(), "file:///etc/passwd", 100); err == nil {
		t.Fatal("file:// should be refused")
	}
}

func TestHTMLText(t *testing.T) {
	got := HTMLText(`<html><head><title>T</title><style>p{}</style></head><body><nav>menu</nav>
<h1>Title</h1><p>One &amp; two</p><script>alert(1)</script><p>Three<br>four</p></body></html>`)
	for _, want := range []string{"Title", "One & two", "Three\nfour"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	for _, bad := range []string{"alert", "menu", "p{}"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q should be stripped: %q", bad, got)
		}
	}
}

func TestSearchDedupesAndLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" || r.URL.Query().Get("q") != "go release" {
			http.Error(rw, "bad", 400)
			return
		}
		rw.Write([]byte(`{"results":[{"title":"a","url":"https://a","content":" x "},{"title":"a2","url":"https://a"},
{"title":"b","url":"https://b"},{"title":"c","url":"https://c"}]}`))
	}))
	defer srv.Close()
	r, err := Search(context.Background(), srv.URL, "go release", 2)
	if err != nil || len(r) != 2 || r[0].URL != "https://a" || r[1].URL != "https://b" || r[0].Snippet != "x" {
		t.Fatalf("%v %+v", err, r)
	}
}
