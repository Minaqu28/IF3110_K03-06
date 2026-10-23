package main

import (
	"context"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/frontend"
	httpserver "github.com/Minaqu28/IF3110_K03-06/internal/http"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	pages, err := template.ParseFS(frontend.Files, "pages/*.html")
	if err != nil {
		return err
	}
	static, err := fs.Sub(frontend.Files, "assets")
	if err != nil {
		return err
	}

	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           httpserver.NewRouter(pages, static),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	stop, done := signal.NotifyContext(context.Background(), os.Interrupt)
	defer done()
	go func() {
		<-stop.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	log.Printf("listening on %s", addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
