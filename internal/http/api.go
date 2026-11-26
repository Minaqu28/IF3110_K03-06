package httpserver

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
)

type apiError struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAPIError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, admin.ErrForbidden):
		writeJSON(w, http.StatusForbidden, apiError{Error: "forbidden"})
	case errors.Is(err, admin.ErrNotFound):
		writeJSON(w, http.StatusNotFound, apiError{Error: "not found"})
	case errors.Is(err, admin.ErrConflict):
		writeJSON(w, http.StatusConflict, apiError{Error: publicMessage(err, admin.ErrConflict)})
	case errors.Is(err, admin.ErrInvalidInput):
		writeJSON(w, http.StatusBadRequest, apiError{Error: publicMessage(err, admin.ErrInvalidInput)})
	default:
		slog.Error("api request failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, apiError{Error: "internal error"})
	}
}

func publicMessage(err, kind error) string {
	message := err.Error()
	if rest, ok := strings.CutPrefix(message, kind.Error()+": "); ok {
		return rest
	}
	return message
}

func (h *AuthenticationHandler) apiSession(w http.ResponseWriter, r *http.Request) (authentication.Session, bool) {
	session, err := h.session(w, r)
	if errors.Is(err, authentication.ErrUnauthenticated) {
		writeJSON(w, http.StatusUnauthorized, apiError{Error: "login required"})
		return authentication.Session{}, false
	}
	if err != nil {
		writeAPIError(w, err)
		return authentication.Session{}, false
	}
	return session, true
}
