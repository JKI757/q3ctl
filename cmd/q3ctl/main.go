// q3ctl is a loopback-only Quake 3 control plane.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"q3ctl/internal/app"
	"q3ctl/internal/config"
)

func main() {
	configPath := flag.String("config", "/etc/q3ctl/config.json", "JSON config path")
	flag.Parse()
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	controller := app.New(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("q3ctl listening on %s", cfg.Listen)
	if err := controller.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
