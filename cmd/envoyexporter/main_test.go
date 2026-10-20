package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const withCTs = `{"production":[
 {"type":"inverters","activeCount":24,"wNow":3010},
 {"type":"eim","activeCount":1,"measurementType":"production","wNow":2987.4,"whLifetime":12345678}],
 "consumption":[
 {"type":"eim","activeCount":1,"measurementType":"total-consumption","wNow":1210.5,"whLifetime":9876543},
 {"type":"eim","activeCount":1,"measurementType":"net-consumption","wNow":-1776.9,"whLifetime":0}]}`

const inverterOnly = `{"production":[{"type":"inverters","activeCount":24,"wNow":512},
 {"type":"eim","activeCount":0,"measurementType":"production","wNow":-3}],"consumption":[]}`

func parse(t *testing.T, s string) Reading {
	var p production
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		t.Fatal(err)
	}
	return Parse(p)
}

func TestParseWithConsumptionCTs(t *testing.T) {
	r := parse(t, withCTs)
	if !r.HasProductionMeter || !r.HasConsumptionMeter || r.ProductionW != 2987.4 || r.ConsumptionW != 1210.5 || r.NetW != -1776.9 {
		t.Fatalf("%+v", r)
	}
}

func TestParseInverterOnlyFallsBackToInverters(t *testing.T) {
	r := parse(t, inverterOnly)
	if r.HasProductionMeter || r.HasConsumptionMeter || r.ProductionW != 512 {
		t.Fatalf("%+v", r)
	}
	var b strings.Builder
	Render(&b, r, nil, time.Millisecond)
	if strings.Contains(b.String(), "envoy_grid_watts") || !strings.Contains(b.String(), "envoy_production_watts 512") {
		t.Fatalf("render without CTs:\n%s", b.String())
	}
}

func TestRenderErrorStillServesUp0(t *testing.T) {
	var b strings.Builder
	Render(&b, Reading{}, errors.New(`bad "token"`), time.Millisecond)
	s := b.String()
	if !strings.Contains(s, "envoy_up 0") || !strings.Contains(s, `envoy_error{reason="bad \"token\""} 1`) {
		t.Fatalf("%s", s)
	}
}

// A gateway that only accepts requests carrying the session cookie set by
// /auth/check_jwt, like some firmware 8 builds.
func TestSessionCookieFlow(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/check_jwt":
			if r.Header.Get("Authorization") != "Bearer tok123" {
				http.Error(rw, "no", http.StatusUnauthorized)
				return
			}
			http.SetCookie(rw, &http.Cookie{Name: "sessionId", Value: "s1", Path: "/"})
			rw.Write([]byte("<h2>Valid token.</h2>"))
		case "/production.json":
			if c, err := r.Cookie("sessionId"); err != nil || c.Value != "s1" {
				http.Error(rw, "no", http.StatusUnauthorized)
				return
			}
			rw.Write([]byte(withCTs))
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	tf := filepath.Join(dir, "token")
	os.WriteFile(tf, []byte("tok123\n"), 0o600)
	c := newClient(srv.URL, tf)
	r, err := c.read(context.Background())
	if err != nil || r.ProductionW != 2987.4 || !c.session {
		t.Fatalf("%v %+v session=%v", err, r, c.session)
	}

	os.WriteFile(tf, []byte("wrong"), 0o600)
	c2 := newClient(srv.URL, tf)
	if _, err := c2.read(context.Background()); !errors.Is(err, errUnauthorized) {
		t.Fatalf("bad token: %v", err)
	}
	c3 := newClient(srv.URL, filepath.Join(dir, "missing"))
	if _, err := c3.read(context.Background()); err == nil {
		t.Fatal("missing token file should fail")
	}
}
