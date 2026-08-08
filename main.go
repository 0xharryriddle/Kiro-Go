// Package main provides the entry point for Kiro API Proxy.
//
// Kiro API Proxy is a reverse proxy service that translates Kiro API requests
// into OpenAI and Anthropic (Claude) compatible formats. Key features include:
//   - Multi-account pool with round-robin load balancing
//   - Automatic OAuth token refresh
//   - Streaming response support for real-time AI interactions
//   - Admin panel for account and configuration management
//
// The service exposes the following endpoints:
//   - /v1/messages - Claude API compatible endpoint
//   - /v1/chat/completions - OpenAI API compatible endpoint
//   - /admin - Web-based administration panel
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"kiro-go/proxy"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// shutdownGrace bounds how long a graceful shutdown waits for in-flight requests
// to finish before connections are closed abruptly.
//
// 30s is a compromise. SSE streams here can legitimately run for minutes, so no
// realistic deadline guarantees every stream completes; waiting indefinitely
// instead would hang a deploy behind one slow client. Docker's default SIGKILL
// timeout after SIGTERM is 10s, so a container stop will usually cut this short
// anyway — set `stop_grace_period` in docker-compose.yml above this value if the
// full drain matters for a given deployment.
const shutdownGrace = 30 * time.Second

func main() {
	// CLI flags. -port/-host let an operator run on a different address without
	// editing config.json (e.g. when 8080 is already taken). Precedence is
	// flag > env (PORT/HOST) > config.json. -1 / "" mean "not set".
	portFlag := flag.Int("port", -1, "HTTP listen port (overrides PORT env and config.json)")
	hostFlag := flag.String("host", "", "HTTP bind host (overrides HOST env and config.json)")
	flag.Parse()

	// 配置文件路径，支持环境变量覆盖
	configPath := "data/config.json"
	if envPath := os.Getenv("CONFIG_PATH"); envPath != "" {
		configPath = envPath
	}

	// 确保数据目录存在
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		log.Fatalf("Failed to create data directory: %v", err)
	}

	// 加载配置
	if err := config.Init(configPath); err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize log level: LOG_LEVEL env var takes priority over config, defaulting to "info".
	logger.Init(config.GetLogLevel())

	// 环境变量覆盖密码
	if envPassword := os.Getenv("ADMIN_PASSWORD"); envPassword != "" {
		config.SetPassword(envPassword)
	}

	// Port/host override: env first, then flag wins if provided. Persisted config
	// is left untouched — these are runtime overrides only.
	if envPort := os.Getenv("PORT"); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil && p > 0 && p <= 65535 {
			config.SetPort(p)
		} else {
			logger.Warnf("Ignoring invalid PORT env value %q", envPort)
		}
	}
	if envHost := os.Getenv("HOST"); envHost != "" {
		config.SetHost(envHost)
	}
	if *portFlag >= 0 {
		if *portFlag == 0 || *portFlag > 65535 {
			log.Fatalf("Invalid -port %d: must be 1-65535", *portFlag)
		}
		config.SetPort(*portFlag)
	}
	if *hostFlag != "" {
		config.SetHost(*hostFlag)
	}
	// Security guard: refuse to boot a publicly-reachable instance that still uses the
	// built-in default admin password. A public VPS that forgets to set ADMIN_PASSWORD
	// would otherwise expose full admin access (account tokens, key creation) to anyone.
	// Loopback-only binds are allowed for local development.
	if config.GetPassword() == "changeme" && !isLoopbackHost(config.GetHost()) {
		log.Fatalf("Refusing to start: admin password is still the default on a non-loopback host (%s). "+
			"Set the ADMIN_PASSWORD environment variable to a strong secret, or bind to 127.0.0.1.", config.GetHost())
	}

	// 初始化账号池
	pool.GetPool()

	// 创建 HTTP 处理器（包含后台刷新任务）
	handler := proxy.NewHandler()

	// 启动服务器
	addr := fmt.Sprintf("%s:%d", config.GetHost(), config.GetPort())
	logger.Infof("Kiro-Go starting on http://%s (log level: %s)", addr, logger.LevelName(logger.GetLevel()))
	logger.Infof("Admin panel: http://%s/admin", addr)
	logger.Infof("Claude API: http://%s/v1/messages", addr)
	logger.Infof("OpenAI API: http://%s/v1/chat/completions", addr)

	// WriteTimeout intentionally 0: SSE streams can run for minutes while the
	// upstream model produces tokens. ReadHeaderTimeout + ReadTimeout still
	// guard against slowloris-style header/body stalls.
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB — cap header size to blunt header-flood / slowloris.
	}

	// Graceful shutdown (round 17). Previously this was a bare ListenAndServe with
	// no signal handling, so `docker compose up -d` sent SIGTERM and the process
	// died instantly: in-flight SSE streams were severed mid-token, and pending
	// stats / prompt-cache / queued trace rows were lost because nothing flushed.
	//
	// signal.NotifyContext cancels ctx on the first SIGINT/SIGTERM. The SECOND
	// signal restores default handling, so an operator who does not want to wait
	// out the drain can always Ctrl-C again and kill it immediately.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ListenAndServe blocks, so it runs on its own goroutine and reports why it
	// returned. A graceful Shutdown makes it return ErrServerClosed, which is the
	// expected path and must not be treated as a failure.
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		// The listener failed on its own (e.g. port already bound). Nothing to
		// drain — but still release Handler resources so the final stats save and
		// cache flush happen before exit.
		if err != nil {
			handler.Close()
			logger.Fatalf("Server failed: %v", err)
		}
		handler.Close()
		return
	case <-ctx.Done():
		logger.Infof("Shutdown signal received; draining in-flight requests (up to %s)", shutdownGrace)
	}

	// stop() restores default signal handling now that the drain has begun, so a
	// second SIGTERM is fatal rather than being swallowed by this handler.
	stop()

	// Bound the drain. An SSE stream can legitimately run for minutes, so this is
	// a deadline, not a promise: Shutdown returns DeadlineExceeded if a stream is
	// still open when the grace period expires, and we log that rather than
	// pretending the shutdown was clean.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warnf("Graceful shutdown incomplete (%v); some connections were closed abruptly", err)
	} else {
		logger.Infof("HTTP server drained cleanly")
	}

	// Stop background loops and flush state AFTER the listener has drained, so any
	// request that completed during the drain is included in the final save.
	handler.Close()
	logger.Infof("Shutdown complete")
}

// isLoopbackHost reports whether the configured bind host is loopback-only, in which
// case the admin panel is not reachable from the network and the default-password
// startup guard can be safely skipped.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
