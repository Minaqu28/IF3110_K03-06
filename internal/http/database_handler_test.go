package httpserver

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/frontend"
	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
)

func renderDatabasePage(t *testing.T, level authorization.AccessLevel) string {
	t.Helper()
	pages, err := template.ParseFS(frontend.Files, "pages/*.html")
	if err != nil {
		t.Fatal(err)
	}
	result := explorer.StatementResult{Command: "SELECT 1", Columns: []string{"id", "name"}, Rows: [][]explorer.Cell{{{Value: "1"}, {Null: true}}}}
	view := explorer.View{
		Connection: domain.Connection{ID: 3, Name: "app"},
		Level:      level,
		Schema:     "public",
		Tables:     []explorer.Table{{Name: "users", Type: "BASE TABLE", Rows: 1}, {Name: "v", Type: "VIEW", Rows: -1}},
		Table:      "users",
		Preview:    &result,
		Results:    []explorer.StatementResult{result, {Error: "ERROR: boom"}},
	}
	var body bytes.Buffer
	data := databasePage{Session: authentication.Session{CSRFToken: "tok"}, View: view}
	if err := pages.ExecuteTemplate(&body, "database.html", data); err != nil {
		t.Fatal(err)
	}
	return body.String()
}

func TestDatabasePageEditorOnlyForViewEdit(t *testing.T) {
	edit := renderDatabasePage(t, authorization.AccessViewEdit)
	for _, want := range []string{`name="sql"`, `value="tok"`, "/databases/3?schema=public&amp;table=users", "NULL", "ERROR: boom"} {
		if !strings.Contains(edit, want) {
			t.Errorf("VIEW_EDIT page missing %q", want)
		}
	}
	if view := renderDatabasePage(t, authorization.AccessViewOnly); strings.Contains(view, `name="sql"`) {
		t.Error("VIEW_ONLY page must not render the SQL editor")
	}
}
