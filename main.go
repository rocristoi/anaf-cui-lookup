package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func main() {
	port := env("PORT", "8080")

	// Used by the Docker HEALTHCHECK, the image has no curl or shell.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		c := http.Client{Timeout: 3 * time.Second}
		res, err := c.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil || res.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	var keys [][32]byte
	for _, k := range strings.Split(os.Getenv("API_KEYS"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, sha256.Sum256([]byte(k)))
		}
	}
	if len(keys) == 0 {
		b := make([]byte, 24)
		rand.Read(b)
		k := hex.EncodeToString(b)
		keys = append(keys, sha256.Sum256([]byte(k)))
		slog.Warn("API_KEYS not set, generated a temporary key. Set API_KEYS to keep it stable.", "api_key", k)
	}

	s := &server{
		anaf:       newAnafClient(time.Duration(envInt("ANAF_TIMEOUT_SECONDS", 12)) * time.Second),
		cache:      newCache(time.Duration(envInt("CACHE_TTL_SECONDS", 3600))*time.Second, 5*time.Minute, 10000),
		keys:       keys,
		rate:       newLimiter(envInt("RATE_LIMIT_PER_MINUTE", 60), time.Minute),
		authFail:   newLimiter(20, time.Minute),
		trustProxy: env("TRUST_PROXY", "false") == "true",
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	go func() {
		slog.Info("listening", "addr", srv.Addr, "keys", len(keys))
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
