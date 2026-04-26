package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/shiyi-jiaqiu/vpsops/internal/execd"
)

func main() {
	base := filepath.Base(os.Args[0])
	switch {
	case strings.Contains(base, "run-child"):
		os.Exit(execd.ChildMain(execd.PrivilegeUser))
	case strings.Contains(base, "root-child"):
		os.Exit(execd.ChildMain(execd.PrivilegeRoot))
	}

	configPath := flag.String("config", "/etc/aiops-execd/config.json", "config file")
	doctor := flag.Bool("doctor", false, "run deployment self-checks and exit")
	doctorProbe := flag.Bool("doctor-probe", false, "with -doctor, probe sudo/helper fd3 execution")
	flag.Parse()

	if *doctor {
		os.Exit(execd.RunDoctor(*configPath, execd.DoctorOptions{ProbeSudo: *doctorProbe}, os.Stdout))
	}

	cfg, err := execd.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	server, err := execd.NewServer(cfg)
	if err != nil {
		log.Fatalf("init server: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("shutting down after %s", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Fatalf("shutdown: %v", err)
		}
	case err := <-errCh:
		if err != nil {
			log.Fatalf("server: %v", err)
		}
	}
}
