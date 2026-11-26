package httpserver

import (
	"net/http"
	"net/url"
	"testing"
)

func TestSetGroupLeaderHandlerAndObservabilityHeader(t *testing.T) {
	router, store := newAdminRouter(t, sessionFor(rootUser))

	form := url.Values{
		"csrf_token": {adminCSRF},
		"group_id":   {"1"},
		"leader_id":  {"2"},
	}
	rec := do(router, http.MethodPost, "/admin/groups/leader", form, true)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin" {
		t.Fatalf("assign leader = %d -> %q, want 303 -> /admin", rec.Code, rec.Header().Get("Location"))
	}
	if rec.Header().Get("X-Correlation-ID") == "" {
		t.Fatal("expected X-Correlation-ID header on response")
	}

	clearForm := url.Values{
		"csrf_token": {adminCSRF},
		"group_id":   {"1"},
		"leader_id":  {""},
	}
	rec2 := do(router, http.MethodPost, "/admin/groups/leader", clearForm, true)
	if rec2.Code != http.StatusSeeOther || rec2.Header().Get("Location") != "/admin" {
		t.Fatalf("clear leader = %d -> %q, want 303 -> /admin", rec2.Code, rec2.Header().Get("Location"))
	}

	nonAdminRouter, _ := newAdminRouter(t, sessionFor(regularUser))
	rec3 := do(nonAdminRouter, http.MethodPost, "/admin/groups/leader", form, true)
	if rec3.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want 403", rec3.Code)
	}

	if len(store.calls) != 2 || store.calls[0] != "SetGroupLeader" || store.calls[1] != "SetGroupLeader" {
		t.Fatalf("unexpected store calls: %v", store.calls)
	}
}
