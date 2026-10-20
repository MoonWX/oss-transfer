package master

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func (c Configuration) agentURL() string {
	if c.AgentURL != "" {
		return c.AgentURL
	}
	return c.PublicURL
}

func validMasterOrigin(value string) bool {
	if strings.ContainsAny(value, "\r\n\x00\t ") {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return false
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return false
		}
	}
	return true
}

func networkData(config Configuration) map[string]string {
	return map[string]string{"publicUrl": config.PublicURL, "agentUrl": config.AgentURL, "effectiveAgentUrl": config.agentURL()}
}

func (s *Server) networkSettings(w http.ResponseWriter, r *http.Request) {
	config, err := s.configuration()
	if err != nil {
		fail(w, 500, "Cannot read Master configuration")
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": networkData(config)})
}

func (s *Server) updateNetworkSettings(w http.ResponseWriter, r *http.Request) {
	var request struct {
		PublicURL *string `json:"publicUrl"`
		AgentURL  *string `json:"agentUrl"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	if request.PublicURL != nil && !validMasterOrigin(*request.PublicURL) {
		fail(w, 400, "Public URL must be an HTTP or HTTPS origin")
		return
	}
	if request.AgentURL != nil && *request.AgentURL != "" && !validMasterOrigin(*request.AgentURL) {
		fail(w, 400, "Agent URL must be an HTTP or HTTPS origin")
		return
	}
	var config Configuration
	err := s.store.Update(func(tx *state.Tx) error {
		if _, err := tx.Get("system", "configuration", &config); err != nil {
			return err
		}
		if request.PublicURL != nil {
			config.PublicURL = strings.TrimRight(*request.PublicURL, "/")
		}
		if request.AgentURL != nil {
			config.AgentURL = strings.TrimRight(*request.AgentURL, "/")
		}
		return tx.Put("system", "configuration", config)
	})
	if err != nil {
		fail(w, 500, "Cannot save Master addresses")
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": networkData(config)})
}
