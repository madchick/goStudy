package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"goChatServer/internal/chat"
	"goChatServer/internal/server"
	"goChatServer/internal/storage"
)

func main() {
	addr := flag.String("addr", getEnv("PORT_ADDR", ":8080"), "HTTP/WebSocket server listen address")
	dbPath := flag.String("db", getEnv("DB_PATH", "chat.db"), "SQLite database file path")
	flag.Parse()

	store, err := storage.NewStore(*dbPath)
	if err != nil {
		log.Fatalf("failed to initialize database (%s): %v", *dbPath, err)
	}
	defer func() {
		_ = store.Close()
	}()

	manager, err := chat.NewManager(store)
	if err != nil {
		log.Fatalf("failed to initialize chat manager: %v", err)
	}

	srvHandler := server.NewServer(store, manager)

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           srvHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("Chat Server listening on %s (DB: %s)", *addr, *dbPath)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-stop
	log.Println("Shutting down server and clearing all rooms...")

	manager.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown error: %v", err)
	}
	log.Println("Server stopped.")
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
