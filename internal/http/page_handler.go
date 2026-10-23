package httpserver

import (
	"bytes"
	"html/template"
	"log"
	"net/http"
)

type PageHandler struct {
	pages *template.Template
}

func (h *PageHandler) Health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func (h *PageHandler) Home(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *PageHandler) Login(w http.ResponseWriter, _ *http.Request) {
	var body bytes.Buffer
	if err := h.pages.ExecuteTemplate(&body, "login.html", nil); err != nil {
		log.Printf("render login: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = body.WriteTo(w)
}
