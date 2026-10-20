// Command rdnagw serves one OpenAI-compatible endpoint in front of a pool of
// llama.cpp workers, routing each request to the worker that most likely has
// its prompt prefix cached.
//
//	rdnagw -config gateway.json
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zazulam/rdna-gateway/internal/gateway"
)

func main() {
	path := flag.String("config", "gateway.json", "config file")
	listen := flag.String("listen", "", "override the config's listen address")
	flag.Parse()

	cfg, err := gateway.LoadConfig(*path)
	if err != nil {
		log.Fatal(err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if k := os.Getenv("RDNAGW_API_KEY"); k != "" {
		cfg.APIKey = k
	}
	g, err := gateway.New(cfg)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go g.Run(ctx)

	srv := &http.Server{Addr: cfg.Listen, Handler: g, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sh)
	}()
	log.Printf("rdnagw listening on %s (%d workers, policy %s)", cfg.Listen, len(cfg.Workers), cfg.Policy)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
