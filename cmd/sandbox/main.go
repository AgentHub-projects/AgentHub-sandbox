package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agenthub-sandbox/internal/app"
	"agenthub-sandbox/internal/config"
)

func main() {
	// 先加载运行配置，再把整个 sandbox 服务装起来。
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	server, cleanup, err := app.New(cfg)
	if err != nil {
		log.Fatalf("create app: %v", err)
	}
	defer cleanup()

	httpServer := &http.Server{
		Addr:              cfg.Address(),
		Handler:           server,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 监听退出信号，给 HTTP 和后台资源一个优雅收尾的机会。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("agenthub-sandbox listening on %s", cfg.Address())
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve http: %v", err)
		}
		return
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		_ = httpServer.Close()
		log.Fatalf("shutdown http: %v", err)
	}
}
