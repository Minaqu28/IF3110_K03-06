package metrics

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type noopDriver struct{}

func (noopDriver) Open(string) (driver.Conn, error) { return nil, errors.New("not used") }

func TestHandlerExposesDBPoolStats(t *testing.T) {
	sql.Register("metrics-noop", noopDriver{})
	db, err := sql.Open("metrics-noop", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewRegistry()
	if err := r.RegisterDBPool(db, "platform"); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterDBPool(db, "platform"); err == nil {
		t.Fatal("registering the same pool twice should fail")
	}

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`go_sql_open_connections{db_name="platform"}`,
		`go_sql_in_use_connections{db_name="platform"}`,
		`go_sql_idle_connections{db_name="platform"}`,
		`go_sql_wait_duration_seconds_total{db_name="platform"}`,
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %s", want)
		}
	}
}
