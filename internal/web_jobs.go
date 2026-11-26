package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	httpserver "github.com/Minaqu28/IF3110_K03-06/internal/http"
	"github.com/Minaqu28/IF3110_K03-06/internal/queue"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
	amqp "github.com/rabbitmq/amqp091-go"
)

func asyncMigrationsEnabled() bool {
	return strings.EqualFold(os.Getenv("ASYNC_MIGRATIONS"), "true")
}

func webJobRoutes(db *sql.DB, auth *httpserver.AuthenticationHandler, adminStore *database.AdminPostgresStore, adminService *admin.AdminService) (func(*http.ServeMux), func(), error) {
	jobs := database.NewJobPostgresStore(db)
	submits := database.NewSubmitPostgresStore(db)
	api := httpserver.NewJobAPI(auth, workflow.NewJobQueryService(adminService, jobs)).
		WithActions(workflow.NewActionAdvisor(adminService, database.NewExecutionPostgresStore(db), submits))
	if !asyncMigrationsEnabled() {
		return api.Routes, func() {}, nil
	}
	url := os.Getenv("AMQP_URL")
	if url == "" {
		return nil, nil, errors.New("AMQP_URL is required when ASYNC_MIGRATIONS=true")
	}
	conn, err := amqp.DialConfig(url, amqp.Config{Dial: amqp.DefaultDial(brokerDialTimeout)})
	if err != nil {
		return nil, nil, fmt.Errorf("connect to the message broker: %w", err)
	}
	publisher, err := queue.NewRabbitPublisher(conn, envOr("AMQP_QUEUE", defaultJobQueue))
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	submit := workflow.NewSubmitService(adminService, database.NewRevisionPostgresStore(db), adminStore,
		submits, publisher, jobs)
	api.WithSubmit(submit)
	return api.Routes, func() {
		_ = publisher.Close()
		_ = conn.Close()
	}, nil
}
