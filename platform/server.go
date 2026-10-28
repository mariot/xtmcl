package platform

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

// Options configures the emulated platform.
type Options struct {
	// Version is returned by /api/settings/version and as the Composer version on
	// ping: the Composer registers again whenever the two differ.
	Version string
	// ManagerID is the manager.id the Composer is configured with.
	ManagerID string
	// ComposerToken authenticates the Composer (its openaev.token).
	ComposerToken string
	// ControlToken authenticates the CLI on the control API.
	ControlToken string
	// PublicKey encrypts the secret settings for the Composer.
	PublicKey *rsa.PublicKey
	// Upstream, when set, receives every request that isn't for the Composer or
	// the control API, e.g. the deployed collectors' calls to OpenAEV.
	Upstream *url.URL
	Logger   *log.Logger
}

// Server emulates the OpenAEV API used by XTM Composer, plus a control API.
type Server struct {
	opts  Options
	store *Store
	mux   *http.ServeMux
}

// NewServer returns the platform handler serving store.
func NewServer(store *Store, opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = log.New(log.Writer(), "", log.LstdFlags)
	}
	s := &Server{opts: opts, store: store, mux: http.NewServeMux()}

	// Composer API (OpenAEV flavour)
	s.mux.HandleFunc("GET /api/settings/version", s.version)
	s.mux.HandleFunc("POST /api/xtm-composer/register", s.composer(s.register))
	s.mux.HandleFunc("PUT /api/xtm-composer/{manager}/refresh-connectivity", s.composer(s.ping))
	s.mux.HandleFunc("GET /api/xtm-composer/{manager}/connector-instances", s.composer(s.instances))
	s.mux.HandleFunc("PUT /api/xtm-composer/{manager}/connector-instances/{id}/status", s.composer(s.status))
	s.mux.HandleFunc("PUT /api/xtm-composer/{manager}/connector-instances/{id}/health-check", s.composer(s.health))
	s.mux.HandleFunc("POST /api/xtm-composer/{manager}/connector-instances/{id}/logs", s.composer(s.logs))
	s.mux.HandleFunc("/api/xtm-composer/", s.composer(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "unknown xtm-composer endpoint")
	}))

	// Control API used by the CLI
	s.mux.HandleFunc("GET /control/status", s.control(s.controlStatus))
	s.mux.HandleFunc("GET /control/connectors", s.control(s.controlList))
	s.mux.HandleFunc("POST /control/connectors", s.control(s.controlUpsert))
	s.mux.HandleFunc("GET /control/connectors/{ref}", s.control(s.controlGet))
	s.mux.HandleFunc("DELETE /control/connectors/{ref}", s.control(s.controlDelete))
	s.mux.HandleFunc("POST /control/connectors/{ref}/start", s.control(s.controlRequest(StatusStarting)))
	s.mux.HandleFunc("POST /control/connectors/{ref}/stop", s.control(s.controlRequest(StatusStopping)))
	s.mux.HandleFunc("GET /control/connectors/{ref}/logs", s.control(s.controlLogs))

	// Everything else goes to the upstream platform, if any
	s.mux.HandleFunc("/", s.fallback())
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]string{"message": message})
}

func bearerMatches(r *http.Request, token string) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// --- Composer API ---

type managerResponse struct {
	ID      string `json:"xtm_composer_id"`
	Version string `json:"xtm_composer_version"`
}

type instanceConfiguration struct {
	Key       string  `json:"configuration_key"`
	Value     *string `json:"configuration_value"`
	Encrypted bool    `json:"configuration_is_encrypted"`
}

type instance struct {
	ID              string                  `json:"connector_instance_id"`
	Name            string                  `json:"connector_instance_name"`
	Hash            string                  `json:"connector_instance_hash"`
	Image           string                  `json:"connector_image"`
	CurrentStatus   string                  `json:"connector_instance_current_status"`
	RequestedStatus string                  `json:"connector_instance_requested_status"`
	Configurations  []instanceConfiguration `json:"connector_instance_configurations"`
}

func toInstance(c Connector) instance {
	configs := make([]instanceConfiguration, 0, len(c.Settings))
	for _, setting := range c.Settings {
		value := setting.Value
		configs = append(configs, instanceConfiguration{Key: setting.Key, Value: &value, Encrypted: setting.Encrypted})
	}
	return instance{
		ID:              c.ID,
		Name:            c.Name,
		Hash:            c.Hash,
		Image:           c.Image,
		CurrentStatus:   c.CurrentStatus,
		RequestedStatus: c.RequestedStatus,
		Configurations:  configs,
	}
}

// composer wraps a Composer endpoint: bearer token and, when the path names a
// manager, the configured manager ID.
func (s *Server) composer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !bearerMatches(r, s.opts.ComposerToken) {
			writeError(w, http.StatusUnauthorized, "invalid or missing Composer token")
			return
		}
		if m := r.PathValue("manager"); m != "" && m != s.opts.ManagerID {
			writeError(w, http.StatusNotFound, "unknown manager "+m)
			return
		}
		next(w, r)
	}
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(s.opts.Version))
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.ID != s.opts.ManagerID {
		writeError(w, http.StatusBadRequest, "unexpected manager id "+in.ID)
		return
	}
	if s.opts.PublicKey != nil {
		pub, err := ParsePublicKeyPEM(in.PublicKey)
		if err != nil || !pub.Equal(s.opts.PublicKey) {
			// Secrets are encrypted with the key the CLI generated: they would be undecryptable
			s.opts.Logger.Printf("WARNING: the Composer registered a public key that doesn't match the generated credentials key; secrets won't decrypt")
		}
	}
	m, err := s.store.Register(in.ID, in.Name, in.PublicKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.opts.Logger.Printf("Composer %q registered (manager %s)", m.Name, m.ID)
	writeJSON(w, http.StatusOK, managerResponse{ID: m.ID, Version: s.opts.Version})
}

func (s *Server) ping(w http.ResponseWriter, r *http.Request) {
	m, err := s.store.Ping(r.PathValue("manager"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, managerResponse{ID: m.ID, Version: s.opts.Version})
}

func (s *Server) instances(w http.ResponseWriter, _ *http.Request) {
	connectors := s.store.Connectors()
	out := make([]instance, 0, len(connectors))
	for _, c := range connectors {
		out = append(out, toInstance(c))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) writeInstance(w http.ResponseWriter, c Connector, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, toInstance(c))
	}
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Status string `json:"connector_instance_current_status"`
	}
	if !decode(w, r, &in) {
		return
	}
	before, _ := s.store.Get(r.PathValue("id"))
	c, err := s.store.SetCurrentStatus(r.PathValue("id"), in.Status)
	if err == nil && before.CurrentStatus != in.Status {
		s.opts.Logger.Printf("Connector %q is %s", c.Name, in.Status)
	}
	s.writeInstance(w, c, err)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RestartCount uint32 `json:"connector_instance_restart_count"`
		StartedAt    string `json:"connector_instance_started_at"`
		RebootLoop   bool   `json:"connector_instance_is_in_reboot_loop"`
	}
	if !decode(w, r, &in) {
		return
	}
	c, err := s.store.SetHealth(r.PathValue("id"), Health{
		RestartCount: in.RestartCount, StartedAt: in.StartedAt, RebootLoop: in.RebootLoop,
	})
	s.writeInstance(w, c, err)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Logs []string `json:"connector_instance_logs"`
	}
	if !decode(w, r, &in) {
		return
	}
	if _, err := s.store.AppendLogs(r.PathValue("id"), in.Logs); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// --- Control API ---

func (s *Server) control(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !bearerMatches(r, s.opts.ControlToken) {
			writeError(w, http.StatusUnauthorized, "invalid or missing control token")
			return
		}
		next(w, r)
	}
}

// Status is the control view of the platform.
type Status struct {
	Version    string   `json:"version"`
	ManagerID  string   `json:"manager_id"`
	Manager    *Manager `json:"manager,omitempty"`
	Connectors int      `json:"connectors"`
	Upstream   string   `json:"upstream,omitempty"`
}

func (s *Server) controlStatus(w http.ResponseWriter, _ *http.Request) {
	st := Status{
		Version:    s.opts.Version,
		ManagerID:  s.opts.ManagerID,
		Manager:    s.store.Manager(),
		Connectors: len(s.store.Connectors()),
	}
	if st.Manager != nil {
		st.Manager.PublicKey = ""
	}
	if s.opts.Upstream != nil {
		st.Upstream = s.opts.Upstream.String()
	}
	writeJSON(w, http.StatusOK, st)
}

// ConnectorView is a connector as returned by the control API: secrets are
// masked and logs are fetched separately.
type ConnectorView struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	ContainerName   string            `json:"container_name"`
	Image           string            `json:"image"`
	Hash            string            `json:"hash"`
	RequestedStatus string            `json:"requested_status"`
	CurrentStatus   string            `json:"current_status"`
	Env             map[string]string `json:"env"`
	Secrets         []string          `json:"secrets"`
	Health          *Health           `json:"health,omitempty"`
	LogLines        int               `json:"log_lines"`
	// Created is set when a declaration created the connector rather than updated it.
	Created bool `json:"created,omitempty"`
}

func view(c Connector) ConnectorView {
	v := ConnectorView{
		ID: c.ID, Name: c.Name, ContainerName: ContainerName(c.Name), Image: c.Image, Hash: c.Hash,
		RequestedStatus: c.RequestedStatus, CurrentStatus: c.CurrentStatus,
		Env: map[string]string{}, Secrets: []string{}, Health: c.Health, LogLines: len(c.Logs),
	}
	for _, setting := range c.Settings {
		if setting.Encrypted {
			v.Secrets = append(v.Secrets, setting.Key)
		} else {
			v.Env[setting.Key] = setting.Value
		}
	}
	return v
}

func (s *Server) controlList(w http.ResponseWriter, _ *http.Request) {
	connectors := s.store.Connectors()
	out := make([]ConnectorView, 0, len(connectors))
	for _, c := range connectors {
		out = append(out, view(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// Definition is the control API input to create or update a connector.
type Definition struct {
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	Env     map[string]string `json:"env"`
	Secrets map[string]string `json:"secrets"`
	Stopped bool              `json:"stopped"`
}

func (s *Server) controlUpsert(w http.ResponseWriter, r *http.Request) {
	var def Definition
	if !decode(w, r, &def) {
		return
	}
	all := map[string]string{}
	var settings []Setting
	for k, v := range def.Env {
		all[k] = v
		settings = append(settings, Setting{Key: k, Value: v})
	}
	for k, v := range def.Secrets {
		if _, dup := def.Env[k]; dup {
			writeError(w, http.StatusBadRequest, k+" is both an env value and a secret")
			return
		}
		if s.opts.PublicKey == nil {
			writeError(w, http.StatusBadRequest, "no credentials key to encrypt secrets with")
			return
		}
		encrypted, err := Encrypt(s.opts.PublicKey, v)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		all[k] = v
		settings = append(settings, Setting{Key: k, Value: encrypted, Encrypted: true})
	}
	requested := StatusStarting
	if def.Stopped {
		requested = StatusStopping
	}
	c, created, err := s.store.Upsert(Connector{
		Name: def.Name, Image: def.Image, Hash: Hash(def.Image, all), Settings: settings, RequestedStatus: requested,
	}, newID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	v := view(c)
	v.Created = created
	code := http.StatusOK
	if created {
		code = http.StatusCreated
		s.opts.Logger.Printf("Connector %q declared (%s)", c.Name, c.Image)
	} else {
		s.opts.Logger.Printf("Connector %q updated (hash %s)", c.Name, c.Hash)
	}
	writeJSON(w, code, v)
}

func (s *Server) controlGet(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.Get(r.PathValue("ref"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view(c))
}

func (s *Server) controlDelete(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.Delete(r.PathValue("ref"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	s.opts.Logger.Printf("Connector %q removed", c.Name)
	writeJSON(w, http.StatusOK, view(c))
}

func (s *Server) controlRequest(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := s.store.SetRequestedStatus(r.PathValue("ref"), status)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		s.opts.Logger.Printf("Connector %q requested %s", c.Name, status)
		writeJSON(w, http.StatusOK, view(c))
	}
}

// LogsPage is a slice of the logs reported for a connector. Next is the offset to
// ask for the following lines; Truncated means older lines were dropped.
type LogsPage struct {
	Lines     []string `json:"lines"`
	Next      int      `json:"next"`
	Truncated bool     `json:"truncated"`
}

func (s *Server) controlLogs(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.Get(r.PathValue("ref"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	since := 0
	if raw := r.URL.Query().Get("since"); raw != "" {
		if since, err = strconv.Atoi(raw); err != nil || since < 0 {
			writeError(w, http.StatusBadRequest, "invalid since")
			return
		}
	}
	page := LogsPage{Lines: []string{}, Next: len(c.Logs)}
	switch {
	case since > len(c.Logs):
		// The kept window moved past this offset: start over from the oldest line
		page.Lines, page.Truncated = c.Logs, true
	default:
		page.Lines = c.Logs[since:]
	}
	writeJSON(w, http.StatusOK, page)
}

// --- Upstream ---

func (s *Server) fallback() http.HandlerFunc {
	if s.opts.Upstream == nil {
		return func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("%s %s: no upstream platform configured", r.Method, r.URL.Path))
		}
	}
	upstream := s.opts.Upstream
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		// Also sends the upstream host as Host header
		r.SetURL(upstream)
	}}
	return proxy.ServeHTTP
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NewToken returns a random hex token.
func NewToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// NewUUID returns a random UUIDv4.
func NewUUID() string { return newID() }
