// envoyexporter serves an Enphase IQ Gateway (Envoy) on the LAN as Prometheus
// metrics: solar production, house consumption and grid import/export, read
// from its local API every scrape.
//
//	ENVOY_URL=https://192.168.1.50 ENVOY_TOKEN_FILE=envoy.token envoyexporter
//
// Firmware 7+ needs an owner token (entrez.enphaseenergy.com, valid a year).
// The gateway uses a self-signed certificate, so TLS verification is skipped
// for that one host. Without a token (or with an expired one) it still serves
// envoy_up 0 and the reason, so Grafana shows "no data" instead of an error.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"sync"
	"time"
)

// Reading is one sample of the gateway's meters, in watts and watt-hours.
type Reading struct {
	ProductionW, ConsumptionW, NetW         float64 // NetW > 0 importing from the grid, < 0 exporting
	ProductionWh, ConsumptionWh             float64 // lifetime
	HasProductionMeter, HasConsumptionMeter bool
	InverterW                               float64 // microinverter total (always present)
}

type client struct {
	base      string
	tokenFile string
	http      *http.Client
	mu        sync.Mutex
	session   bool // a sessionId cookie from /auth/check_jwt is in the jar
}

func newClient(base, tokenFile string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{
		base:      strings.TrimRight(base, "/"),
		tokenFile: tokenFile,
		http: &http.Client{
			Timeout: 10 * time.Second,
			Jar:     jar,
			Transport: &http.Transport{
				// The gateway's certificate is self-signed for its serial number.
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // single LAN host
			},
		},
	}
}

func (c *client) token() (string, error) {
	if c.tokenFile == "" {
		return "", errors.New("ENVOY_TOKEN_FILE not set")
	}
	b, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errors.New("token file is empty")
	}
	return t, nil
}

var errUnauthorized = errors.New("gateway rejected the token (expired or for another gateway?)")

// get fetches path as JSON. Firmware 7/8 accepts the bearer token directly on
// most builds; some only after /auth/check_jwt has set a session cookie, so on
// a 401 it does that once and retries.
func (c *client) get(ctx context.Context, path string, into any) error {
	tok, err := c.token()
	if err != nil {
		return err
	}
	do := func() (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		return c.http.Do(req)
	}
	resp, err := do()
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		c.mu.Lock()
		err = c.checkJWT(ctx, tok)
		c.mu.Unlock()
		if err != nil {
			return err
		}
		if resp, err = do(); err != nil {
			return err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(into)
}

func (c *client) checkJWT(ctx context.Context, tok string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/auth/check_jwt", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errUnauthorized
	}
	c.session = true
	return nil
}

// production.json?details=1: "production" holds the inverter total and (with
// CTs) an "eim" production meter; "consumption" holds eim total-consumption and
// net-consumption meters when consumption CTs are installed.
type meter struct {
	Type            string  `json:"type"`
	MeasurementType string  `json:"measurementType"`
	ActiveCount     int     `json:"activeCount"`
	WNow            float64 `json:"wNow"`
	WhLifetime      float64 `json:"whLifetime"`
}

type production struct {
	Production  []meter `json:"production"`
	Consumption []meter `json:"consumption"`
}

// Parse turns production.json into a Reading.
func Parse(p production) Reading {
	var r Reading
	for _, m := range p.Production {
		switch {
		case m.Type == "inverters":
			r.InverterW = m.WNow
		case m.Type == "eim" && m.ActiveCount > 0:
			r.HasProductionMeter = true
			r.ProductionW, r.ProductionWh = m.WNow, m.WhLifetime
		}
	}
	if !r.HasProductionMeter {
		r.ProductionW = r.InverterW
	}
	for _, m := range p.Consumption {
		if m.Type != "eim" || m.ActiveCount == 0 {
			continue
		}
		switch m.MeasurementType {
		case "total-consumption":
			r.HasConsumptionMeter = true
			r.ConsumptionW, r.ConsumptionWh = m.WNow, m.WhLifetime
		case "net-consumption":
			r.NetW = m.WNow
		}
	}
	// Small negative production readings at night are meter noise.
	if r.ProductionW < 0 {
		r.ProductionW = 0
	}
	return r
}

func (c *client) read(ctx context.Context) (Reading, error) {
	var p production
	if err := c.get(ctx, "/production.json?details=1", &p); err != nil {
		return Reading{}, err
	}
	return Parse(p), nil
}

// Render writes the Prometheus text for a reading (or an error).
func Render(w io.Writer, r Reading, err error, took time.Duration) {
	up := 1
	if err != nil {
		up = 0
	}
	fmt.Fprintf(w, "# TYPE envoy_up gauge\nenvoy_up %d\n", up)
	fmt.Fprintf(w, "# TYPE envoy_scrape_seconds gauge\nenvoy_scrape_seconds %.3f\n", took.Seconds())
	if err != nil {
		// %q escapes backslashes, quotes and newlines the way label values need.
		fmt.Fprintf(w, "# TYPE envoy_error gauge\nenvoy_error{reason=%q} 1\n", err.Error())
		return
	}
	g := func(name, help string, v float64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
	}
	g("envoy_production_watts", "Solar production now", r.ProductionW)
	g("envoy_inverter_watts", "Microinverter total now", r.InverterW)
	b := func(v bool) float64 {
		if v {
			return 1
		}
		return 0
	}
	g("envoy_has_consumption_meter", "1 if consumption CTs are installed", b(r.HasConsumptionMeter))
	if r.HasProductionMeter {
		fmt.Fprintf(w, "# TYPE envoy_production_wh_total counter\nenvoy_production_wh_total %g\n", r.ProductionWh)
	}
	if r.HasConsumptionMeter {
		g("envoy_consumption_watts", "Whole-house consumption now", r.ConsumptionW)
		g("envoy_grid_watts", "Grid power now: positive = importing, negative = exporting", r.NetW)
		fmt.Fprintf(w, "# TYPE envoy_consumption_wh_total counter\nenvoy_consumption_wh_total %g\n", r.ConsumptionWh)
	}
}

func main() {
	base := os.Getenv("ENVOY_URL")
	if base == "" {
		base = "https://envoy.local"
	}
	listen := os.Getenv("LISTEN")
	if listen == "" {
		listen = "127.0.0.1:9110"
	}
	c := newClient(base, os.Getenv("ENVOY_TOKEN_FILE"))
	http.HandleFunc("/metrics", func(rw http.ResponseWriter, req *http.Request) {
		start := time.Now()
		ctx, cancel := context.WithTimeout(req.Context(), 12*time.Second)
		defer cancel()
		r, err := c.read(ctx)
		if err != nil {
			log.Printf("read: %v", err)
		}
		rw.Header().Set("Content-Type", "text/plain; version=0.0.4")
		Render(rw, r, err, time.Since(start))
	})
	log.Printf("envoyexporter on %s for %s", listen, base)
	log.Fatal((&http.Server{Addr: listen, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe())
}
