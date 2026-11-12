package webserver

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"mattermost-locker/internal/webserver/command"
)

const maxBodyBytes = 64 << 10

func (s *Server) commandEndpoint(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// Authenticate request.
	if !s.validateMattermostRequest(r) {
		slog.Warn("Mattermost authentication failed",
			"command", r.FormValue("command"),
			"form_token_present", r.FormValue("token") != "",
			"auth_header_present", r.Header.Get("Authorization") != "",
		)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	req := &command.Request{
		Command:     r.FormValue("command"),
		Text:        r.FormValue("text"),
		UserName:    r.FormValue("user_name"),
		UserID:      r.FormValue("user_id"),
		ChannelID:   r.FormValue("channel_id"),
		TeamID:      r.FormValue("team_id"),
		ResponseURL: r.FormValue("response_url"),
		Token:       r.FormValue("token"),
	}

	slog.Info("Mattermost command received",
		"command", req.Command,
		"text", req.Text,
		"user", req.UserName,
		"user_id", req.UserID,
		"channel_id", req.ChannelID,
	)

	response, err := s.dispatcher.Dispatch(r.Context(), req)
	if err != nil {
		slog.Error("Command execution failed",
			"command", req.Command,
			"text", req.Text,
			"error", err,
		)
		response = command.Ephemeral("❌ An error occurred while processing your command.")
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		slog.Error("Failed to encode response", "error", err)
	}
}

func (s *Server) validateMattermostRequest(r *http.Request) bool {
	received := ""

	// Check Authorization header.
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) == 2 &&
		(strings.EqualFold(parts[0], "Bearer") ||
			strings.EqualFold(parts[0], "Token")) {
		received = parts[1]
	}

	// Fall back to the form token.
	if received == "" {
		received = r.FormValue("token")
	}

	return subtle.ConstantTimeCompare([]byte(s.token), []byte(received)) == 1
}
