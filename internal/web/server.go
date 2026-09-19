package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"cybercompanion/internal/config"
	"cybercompanion/internal/hal"
	"cybercompanion/internal/persona"
	"cybercompanion/internal/qq"
)

//go:embed all:static
var staticFS embed.FS

// StartServer starts the embedded web dashboard server
func StartServer(port int) error {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return fmt.Errorf("failed to locate embedded static assets: %w", err)
	}

	mux := http.NewServeMux()

	// REST APIs
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/config", handleConfig)
	mux.HandleFunc("/api/persona", handlePersona)
	mux.HandleFunc("/api/logs", handleLogs)
	mux.HandleFunc("/api/restart", handleRestart)

	// Static files handler
	fileServer := http.FileServer(http.FS(sub))
	mux.Handle("/", fileServer)

	addr := ":" + strconv.Itoa(port)
	qq.AddLog("[WebUI] Embedded Dashboard listening on http://0.0.0.0%s", addr)

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	return srv.ListenAndServe()
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := config.Get()
	driver := hal.GetDriver()
	devInfo := driver.GetInfo()

	name, prompt := persona.GetActivePersona()
	title := "专属伴侣"
	desc := prompt
	for _, p := range persona.GetAllPresets() {
		if p.ID == cfg.ActivePersona {
			title = p.Title
			desc = p.Description
			break
		}
	}

	resp := map[string]interface{}{
		"device_info":    devInfo,
		"bot_name":       name,
		"system_prompt":  prompt,
		"active_persona": cfg.ActivePersona,
		"persona_title":  title,
		"persona_desc":   desc,
		"qq_connected":   qq.IsWSConnected(),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := config.Get()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cfg)

	case http.MethodPost:
		var updateReq struct {
			QQAppID        string `json:"qq_appid"`
			QQSecret       string `json:"qq_secret"`
			OneAPIURL      string `json:"oneapi_url"`
			OneAPIToken    string `json:"oneapi_token"`
			Model          string `json:"model"`
			Passcode       string `json:"passcode"`
			WebPort        int    `json:"web_port"`
			EnableStickers bool   `json:"enable_stickers"`
		}

		if err := json.NewDecoder(r.Body).Decode(&updateReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		err := config.Update(func(c *config.Config) {
			if updateReq.QQAppID != "" {
				c.QQAppID = updateReq.QQAppID
			}
			if updateReq.QQSecret != "" {
				c.QQSecret = updateReq.QQSecret
			}
			if updateReq.OneAPIURL != "" {
				c.OneAPIURL = updateReq.OneAPIURL
			}
			if updateReq.OneAPIToken != "" {
				c.OneAPIToken = updateReq.OneAPIToken
			}
			if updateReq.Model != "" {
				c.Model = updateReq.Model
			}
			if updateReq.Passcode != "" {
				c.Passcode = updateReq.Passcode
			}
			if updateReq.WebPort > 0 {
				c.WebPort = updateReq.WebPort
			}
			c.EnableStickers = updateReq.EnableStickers
		})

		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		qq.AddLog("[Config] Configuration updated and saved via WebUI")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handlePersona(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := config.Get()
		presets := persona.GetAllPresets()
		resp := map[string]interface{}{
			"active_persona": cfg.ActivePersona,
			"presets":        presets,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)

	case http.MethodPost:
		var req struct {
			ID     string `json:"id"`
			Name   string `json:"name,omitempty"`
			Prompt string `json:"prompt,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var err error
		if req.ID == "custom" {
			err = config.Update(func(c *config.Config) {
				c.ActivePersona = "custom"
				if req.Name != "" {
					c.BotName = req.Name
				}
				if req.Prompt != "" {
					c.SystemPrompt = req.Prompt
				}
			})
		} else {
			err = persona.SetPersona(req.ID, "")
		}

		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		qq.AddLog("[Persona] Switched persona to: %s", req.ID)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	logs := qq.GetRecentLogs()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(logs)
}

func handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	qq.AddLog("[System] WebUI requested device restart")
	go func() {
		time.Sleep(3 * time.Second)
		driver := hal.GetDriver()
		_, _ = driver.ExecuteRootCmd("reboot")
	}()

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"rebooting"}`))
}
