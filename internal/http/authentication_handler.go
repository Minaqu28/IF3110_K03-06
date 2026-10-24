package httpserver

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"strings"

	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
)

const sessionCookie = "cookie_session"
const loginCSRFCookie = "cookie_login_csrf"

type AuthenticationService interface {
	Login(context.Context, string, string) (string, error)
	Validate(context.Context, string) (authentication.Session, error)
	Logout(context.Context, string) error
}

type AuthenticationHandler struct {
	service       AuthenticationService
	templates     *template.Template
	secureCookies bool
	loginCSRFKey  []byte
}

func NewAuthenticationHandler(service AuthenticationService, templates *template.Template, secureCookies bool) (*AuthenticationHandler, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return &AuthenticationHandler{service: service, templates: templates, secureCookies: secureCookies, loginCSRFKey: key}, nil
}

func (h *AuthenticationHandler) Home(w http.ResponseWriter, r *http.Request) {
	if _, err := h.session(w, r); err == nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	} else if !errors.Is(err, authentication.ErrUnauthenticated) {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *AuthenticationHandler) ShowLogin(w http.ResponseWriter, r *http.Request) {
	if _, err := h.session(w, r); err == nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	} else if !errors.Is(err, authentication.ErrUnauthenticated) {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.renderLogin(w, "", http.StatusOK)
}

func (h *AuthenticationHandler) renderLogin(w http.ResponseWriter, message string, status int) {
	token, err := h.newLoginCSRF()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.setCookie(w, loginCSRFCookie, token, "/login", 600)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = h.templates.ExecuteTemplate(w, "login.html", struct{ Message, CSRF string }{message, token})
}

func (h *AuthenticationHandler) Login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if !h.verifyLoginCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	identifier, err := h.service.Login(r.Context(), r.PostForm.Get("username"), r.PostForm.Get("password"))
	if errors.Is(err, authentication.ErrInvalidCredentials) {
		h.renderLogin(w, "Username atau password salah.", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.clearCookie(w, loginCSRFCookie, "/login")
	h.setCookie(w, sessionCookie, identifier, "/", 0)
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (h *AuthenticationHandler) session(w http.ResponseWriter, r *http.Request) (authentication.Session, error) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return authentication.Session{}, authentication.ErrUnauthenticated
	}
	session, err := h.service.Validate(r.Context(), cookie.Value)
	if err != nil {
		if errors.Is(err, authentication.ErrUnauthenticated) {
			h.clearCookie(w, sessionCookie, "/")
		}
		return authentication.Session{}, err
	}
	if session.RotatedIdentifier != "" {
		h.setCookie(w, sessionCookie, session.RotatedIdentifier, "/", 0)
	}
	return session, nil
}

func (h *AuthenticationHandler) requireSession(w http.ResponseWriter, r *http.Request) (authentication.Session, bool) {
	session, err := h.session(w, r)
	if errors.Is(err, authentication.ErrUnauthenticated) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return authentication.Session{}, false
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return authentication.Session{}, false
	}
	return session, true
}

func (h *AuthenticationHandler) Logout(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireSession(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if !sameToken(r.PostForm.Get("csrf_token"), session.CSRFToken) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	cookie, _ := r.Cookie(sessionCookie)
	if err := h.service.Logout(r.Context(), cookie.Value); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.clearCookie(w, sessionCookie, "/")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *AuthenticationHandler) ShowDashboard(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireSession(w, r)
	if !ok {
		return
	}
	h.render(w, "dashboard.html", session)
}

func (h *AuthenticationHandler) render(w http.ResponseWriter, name string, session authentication.Session) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.templates.ExecuteTemplate(w, name, session); err != nil {
		return
	}
}

func (h *AuthenticationHandler) setCookie(w http.ResponseWriter, name, value, path string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: path, MaxAge: maxAge, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: h.secureCookies})
}

func (h *AuthenticationHandler) clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{Name: name, Path: path, MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: h.secureCookies})
}

func (h *AuthenticationHandler) newLoginCSRF() (string, error) {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	value := base64.RawURLEncoding.EncodeToString(nonce)
	mac := hmac.New(sha256.New, h.loginCSRFKey)
	_, _ = mac.Write([]byte(value))
	return value + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (h *AuthenticationHandler) verifyLoginCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(loginCSRFCookie)
	if err != nil || !sameToken(cookie.Value, r.PostForm.Get("csrf_token")) {
		return false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 {
		return false
	}
	mac := hmac.New(sha256.New, h.loginCSRFKey)
	_, _ = mac.Write([]byte(parts[0]))
	claimed, err := base64.RawURLEncoding.DecodeString(parts[1])
	return err == nil && hmac.Equal(claimed, mac.Sum(nil))
}

func sameToken(a, b string) bool { return a != "" && b != "" && hmac.Equal([]byte(a), []byte(b)) }
