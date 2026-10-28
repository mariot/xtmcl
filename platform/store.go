package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Requested statuses understood by the Composer.
const (
	StatusStarting = "starting"
	StatusStopping = "stopping"
)

// maxLogLines caps the logs kept per connector.
const maxLogLines = 2000

// Setting is one connector configuration value. Value holds the ciphertext when
// Encrypted is set: secrets are never stored in clear.
type Setting struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Encrypted bool   `json:"encrypted"`
}

// Health is the last health report sent by the Composer for a connector.
type Health struct {
	RestartCount uint32    `json:"restart_count"`
	StartedAt    string    `json:"started_at"`
	RebootLoop   bool      `json:"reboot_loop"`
	ReportedAt   time.Time `json:"reported_at"`
}

// Connector is a connector instance managed by the Composer.
type Connector struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Image           string    `json:"image"`
	Hash            string    `json:"hash"`
	RequestedStatus string    `json:"requested_status"`
	CurrentStatus   string    `json:"current_status"`
	Settings        []Setting `json:"settings"`
	Health          *Health   `json:"health,omitempty"`
	Logs            []string  `json:"logs,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Manager is what the platform knows about the Composer registered on it.
type Manager struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	PublicKey    string    `json:"public_key"`
	RegisteredAt time.Time `json:"registered_at"`
	LastPingAt   time.Time `json:"last_ping_at"`
}

type state struct {
	Manager    *Manager     `json:"manager,omitempty"`
	Connectors []*Connector `json:"connectors"`
}

// Store keeps the platform state in memory and persists it to a JSON file.
type Store struct {
	mu   sync.Mutex
	path string
	st   state
	now  func() time.Time
}

// ErrNotFound is returned for an unknown connector.
var ErrNotFound = errors.New("connector not found")

// OpenStore loads the state at path, or starts empty when the file doesn't exist.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, now: time.Now}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, &s.st); err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}
	return s, nil
}

func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ContainerName is the name the Composer gives the deployment of a connector:
// the lowercased name with every non-alphanumeric character replaced by '-'.
func ContainerName(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return '-'
	}, name)
}

// Hash identifies a connector definition: the Composer redeploys a connector
// when its hash changes. Secrets are hashed in clear, before encryption.
func Hash(image string, settings map[string]string) string {
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(image))
	for _, k := range keys {
		_, _ = fmt.Fprintf(h, "\x00%s=%s", k, settings[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func clone(c *Connector) Connector {
	out := *c
	out.Settings = append([]Setting(nil), c.Settings...)
	out.Logs = append([]string(nil), c.Logs...)
	if c.Health != nil {
		h := *c.Health
		out.Health = &h
	}
	return out
}

func (s *Store) find(ref string) *Connector {
	for _, c := range s.st.Connectors {
		if c.ID == ref || c.Name == ref {
			return c
		}
	}
	return nil
}

// Connectors returns a copy of every connector, ordered by name.
func (s *Store) Connectors() []Connector {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Connector, 0, len(s.st.Connectors))
	for _, c := range s.st.Connectors {
		out = append(out, clone(c))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the connector with this ID or name.
func (s *Store) Get(ref string) (Connector, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.find(ref)
	if c == nil {
		return Connector{}, ErrNotFound
	}
	return clone(c), nil
}

// Upsert creates the connector, or replaces the definition of the one with the
// same name. Its deployment name must not clash with another connector's.
func (s *Store) Upsert(def Connector, newID func() string) (Connector, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(def.Name) == "" {
		return Connector{}, false, errors.New("name is required")
	}
	if strings.TrimSpace(def.Image) == "" {
		return Connector{}, false, errors.New("image is required")
	}
	for _, c := range s.st.Connectors {
		if c.Name != def.Name && ContainerName(c.Name) == ContainerName(def.Name) {
			return Connector{}, false, fmt.Errorf("name %q clashes with connector %q: both deploy as %q",
				def.Name, c.Name, ContainerName(def.Name))
		}
	}
	now := s.now()
	existing := s.find(def.Name)
	created := existing == nil
	if created {
		existing = &Connector{ID: newID(), Name: def.Name, CurrentStatus: "stopped", CreatedAt: now}
		s.st.Connectors = append(s.st.Connectors, existing)
	}
	existing.Image = def.Image
	existing.Hash = def.Hash
	existing.Settings = def.Settings
	existing.RequestedStatus = def.RequestedStatus
	existing.UpdatedAt = now
	return clone(existing), created, s.save()
}

// SetRequestedStatus asks the Composer to start or stop a connector.
func (s *Store) SetRequestedStatus(ref, status string) (Connector, error) {
	if status != StatusStarting && status != StatusStopping {
		return Connector{}, fmt.Errorf("invalid requested status %q", status)
	}
	return s.update(ref, func(c *Connector) {
		c.RequestedStatus = status
		c.UpdatedAt = s.now()
	})
}

// SetCurrentStatus records the status reported by the Composer.
func (s *Store) SetCurrentStatus(ref, status string) (Connector, error) {
	return s.update(ref, func(c *Connector) { c.CurrentStatus = status })
}

// SetHealth records a health report from the Composer.
func (s *Store) SetHealth(ref string, h Health) (Connector, error) {
	return s.update(ref, func(c *Connector) {
		h.ReportedAt = s.now()
		c.Health = &h
	})
}

// AppendLogs adds logs reported by the Composer, keeping the last maxLogLines.
// The Composer sends a snapshot of the latest lines each time (the last 100 with
// Docker, all of them with Kubernetes), so only the lines after the overlap with
// the kept ones are new.
func (s *Store) AppendLogs(ref string, snapshot []string) (Connector, error) {
	// Docker log frames keep their line terminator
	trimmed := make([]string, len(snapshot))
	for i, line := range snapshot {
		trimmed[i] = strings.TrimRight(line, "\r\n")
	}
	snapshot = trimmed
	return s.update(ref, func(c *Connector) {
		if len(snapshot) > maxLogLines {
			snapshot = snapshot[len(snapshot)-maxLogLines:]
		}
		c.Logs = append(c.Logs, snapshot[overlap(c.Logs, snapshot):]...)
		if extra := len(c.Logs) - maxLogLines; extra > 0 {
			c.Logs = append([]string(nil), c.Logs[extra:]...)
		}
	})
}

// overlap returns the length of the longest suffix of kept that starts snapshot.
func overlap(kept, snapshot []string) int {
	for n := min(len(kept), len(snapshot)); n > 0; n-- {
		if slices.Equal(kept[len(kept)-n:], snapshot[:n]) {
			return n
		}
	}
	return 0
}

func (s *Store) update(ref string, change func(*Connector)) (Connector, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.find(ref)
	if c == nil {
		return Connector{}, ErrNotFound
	}
	change(c)
	return clone(c), s.save()
}

// Delete removes a connector: the Composer then removes its deployment.
func (s *Store) Delete(ref string) (Connector, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.st.Connectors {
		if c.ID == ref || c.Name == ref {
			s.st.Connectors = append(s.st.Connectors[:i], s.st.Connectors[i+1:]...)
			return clone(c), s.save()
		}
	}
	return Connector{}, ErrNotFound
}

// Register records the Composer registration.
func (s *Store) Register(id, name, publicKey string) (Manager, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.st.Manager = &Manager{ID: id, Name: name, PublicKey: publicKey, RegisteredAt: now, LastPingAt: now}
	return *s.st.Manager, s.save()
}

// Ping records a Composer ping; it fails when the Composer never registered.
func (s *Store) Ping(id string) (Manager, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Manager == nil || s.st.Manager.ID != id {
		return Manager{}, errors.New("manager not registered")
	}
	s.st.Manager.LastPingAt = s.now()
	return *s.st.Manager, s.save()
}

// Manager returns the registered Composer, if any.
func (s *Store) Manager() *Manager {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Manager == nil {
		return nil
	}
	m := *s.st.Manager
	return &m
}
