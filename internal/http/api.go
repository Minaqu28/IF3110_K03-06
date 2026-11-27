package httpserver

import (
	"encoding/json"
	"net/http"
	"strings"
)

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func publicMessage(err, kind error) string {
	message := err.Error()
	if rest, ok := strings.CutPrefix(message, kind.Error()+": "); ok {
		return rest
	}
	return message
}
