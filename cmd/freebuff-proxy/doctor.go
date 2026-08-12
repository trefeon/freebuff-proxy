package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/registry"
)

func runDoctor(configPath string) {
	fmt.Println("freebuff-proxy doctor diagnostic tool")
	fmt.Println("=====================================")

	passed := 0
	warnings := 0
	failed := 0

	ok := func(msg string) {
		fmt.Printf("[ok] %s\n", msg)
		passed++
	}
	warn := func(msg string) {
		fmt.Printf("[!!] %s\n", msg)
		warnings++
	}
	fail := func(msg string) {
		fmt.Printf("[FAIL] %s\n", msg)
		failed++
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fail(fmt.Sprintf("Config loading failed: %v", err))
		fmt.Printf("\nSummary: %d passed, %d warnings, %d failed\n", passed, warnings, failed)
		os.Exit(1)
	}
	ok("Configuration loaded & validated successfully")

	if cfg.BridgeMode() {
		warn("AUTH_TOKENS is empty (bridge mode active). Clients must supply Authorization: Bearer <token>")
	} else {
		ok(fmt.Sprintf("AUTH_TOKENS: %d token(s) configured", len(cfg.AuthTokens)))
		for i, tok := range cfg.AuthTokens {
			if strings.HasPrefix(strings.ToLower(tok), "bearer ") {
				warn(fmt.Sprintf("Token #%d starts with 'Bearer ' prefix -- remove it from .env", i+1))
			} else if tok == "cb_xxx" || tok == "cb_yyy" {
				warn(fmt.Sprintf("Token #%d is a placeholder string %q", i+1, tok))
			} else {
				ok(fmt.Sprintf("Token #%d format valid (%d chars)", i+1, len(tok)))
			}
		}
	}

	// Port availability check
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		fail(fmt.Sprintf("Listen address %s is not available: %v", cfg.ListenAddr, err))
	} else {
		_ = ln.Close()
		ok(fmt.Sprintf("Listen address %s is available", cfg.ListenAddr))
	}

	// DNS & TLS reachability check
	targetHost := "www.codebuff.com"
	if u, err := url.Parse(cfg.UpstreamBaseURL); err == nil && u.Host != "" {
		targetHost = u.Host
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupHost(ctx, targetHost)
	if err != nil {
		fail(fmt.Sprintf("DNS lookup for %s failed: %v", targetHost, err))
	} else {
		ok(fmt.Sprintf("DNS lookup for %s resolved (%s)", targetHost, strings.Join(addrs, ", ")))
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tlsConn, err := tls.DialWithDialer(dialer, "tcp", targetHost+":443", &tls.Config{ServerName: targetHost})
	if err != nil {
		fail(fmt.Sprintf("TLS connection to %s:443 failed: %v", targetHost, err))
	} else {
		_ = tlsConn.Close()
		ok(fmt.Sprintf("TLS connection to %s:443 succeeded", targetHost))
	}

	// Registry test
	reg := registry.New(&cfg, &http.Client{Timeout: 10 * time.Second})
	reg.LoadFallback()
	ok(fmt.Sprintf("Model registry offline fallback loaded (%d models, %d agents)", reg.ModelCount(), len(reg.AgentIDs())))

	if err := reg.Refresh(ctx); err != nil {
		warn(fmt.Sprintf("Registry live refresh warning: %v (offline fallback retained)", err))
	} else {
		ok(fmt.Sprintf("Registry live refresh succeeded (%d models)", reg.ModelCount()))
	}

	fmt.Printf("\nSummary: %d passed, %d warnings, %d failed\n", passed, warnings, failed)
	if failed > 0 {
		os.Exit(1)
	}
	os.Exit(0)
}
