package platform

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	managerID     = "manager-1"
	composerToken = "composer-token"
	controlToken  = "control-token"
)

var (
	keyOnce sync.Once
	keyPath string
	testKey *rsa.PrivateKey
)

// sharedKey generates the 4096-bit test key once: it is slow to generate.
func sharedKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	keyOnce.Do(func() {
		dir, err := os.MkdirTemp("", "xtmcl-key")
		if err != nil {
			t.Fatal(err)
		}
		keyPath = filepath.Join(dir, "key.pem")
		if err := GenerateKey(keyPath); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(keyPath)
		block, _ := pem.Decode(data)
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		testKey = parsed.(*rsa.PrivateKey)
	})
	return testKey, keyPath
}

type harness struct {
	t      *testing.T
	store  *Store
	server *httptest.Server
	logs   *bytes.Buffer
}

func newHarness(t *testing.T, upstream *url.URL) *harness {
	t.Helper()
	key, _ := sharedKey(t)
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	srv := NewServer(store, Options{
		Version: "test-1", ManagerID: managerID, ComposerToken: composerToken, ControlToken: controlToken,
		PublicKey: &key.PublicKey, Upstream: upstream, Logger: log.New(logs, "", 0),
	})
	h := &harness{t: t, store: store, server: httptest.NewServer(srv), logs: logs}
	t.Cleanup(h.server.Close)
	return h
}

func (h *harness) do(method, path, token string, body any) (int, []byte) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func decodeAs[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return v
}

func publicKeyPEM(t *testing.T, pub *rsa.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// decryptLikeComposer mirrors parse_aes_encrypted_value in xtm-composer
// (src/api/decrypt_value.rs) for version 2.
func decryptLikeComposer(t *testing.T, key *rsa.PrivateKey, value string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 513 {
		t.Fatalf("encrypted value too short: %d bytes", len(raw))
	}
	if raw[0] != 2 {
		t.Fatalf("version %d, want 2", raw[0])
	}
	keyIV, err := rsa.DecryptOAEP(sha256.New(), nil, key, raw[1:513], nil)
	if err != nil {
		t.Fatalf("RSA-OAEP: %v", err)
	}
	block, _ := aes.NewCipher(keyIV[0:32])
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, keyIV[32:44], raw[513:], nil)
	if err != nil {
		t.Fatalf("AES-GCM: %v", err)
	}
	return string(plain)
}

func TestComposerEndpointsRequireTheComposerToken(t *testing.T) {
	h := newHarness(t, nil)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/xtm-composer/register"},
		{"PUT", "/api/xtm-composer/manager-1/refresh-connectivity"},
		{"GET", "/api/xtm-composer/manager-1/connector-instances"},
		{"PUT", "/api/xtm-composer/manager-1/connector-instances/x/status"},
	} {
		for _, token := range []string{"", "wrong", controlToken} {
			if code, _ := h.do(tc.method, tc.path, token, nil); code != http.StatusUnauthorized {
				t.Errorf("%s %s with token %q: got %d, want 401", tc.method, tc.path, token, code)
			}
		}
	}
	if code, _ := h.do("GET", "/control/connectors", composerToken, nil); code != http.StatusUnauthorized {
		t.Errorf("control API with the Composer token: got %d, want 401", code)
	}
}

func TestVersionRegisterAndPingAgree(t *testing.T) {
	h := newHarness(t, nil)
	key, _ := sharedKey(t)

	code, body := h.do("GET", "/api/settings/version", "", nil)
	if code != http.StatusOK || string(body) != "test-1" {
		t.Fatalf("version: %d %q", code, body)
	}

	// Ping before registering fails, as on the platform
	if code, _ := h.do("PUT", "/api/xtm-composer/manager-1/refresh-connectivity", composerToken, nil); code != http.StatusNotFound {
		t.Fatalf("ping before register: got %d, want 404", code)
	}

	register := map[string]string{"id": managerID, "name": "test composer", "public_key": publicKeyPEM(t, &key.PublicKey)}
	code, body = h.do("POST", "/api/xtm-composer/register", composerToken, register)
	if code != http.StatusOK {
		t.Fatalf("register: %d %s", code, body)
	}
	m := decodeAs[map[string]string](t, body)
	if m["xtm_composer_id"] != managerID || m["xtm_composer_version"] != "test-1" {
		t.Fatalf("register response %v", m)
	}

	// The Composer registers again whenever the ping version differs from /settings/version
	code, body = h.do("PUT", "/api/xtm-composer/manager-1/refresh-connectivity", composerToken, nil)
	if code != http.StatusOK || decodeAs[map[string]string](t, body)["xtm_composer_version"] != "test-1" {
		t.Fatalf("ping: %d %s", code, body)
	}
	if strings.Contains(h.logs.String(), "WARNING") {
		t.Fatalf("unexpected warning: %s", h.logs)
	}

	// Another manager ID is unknown
	if code, _ := h.do("GET", "/api/xtm-composer/other/connector-instances", composerToken, nil); code != http.StatusNotFound {
		t.Fatalf("other manager: got %d, want 404", code)
	}
}

func TestRegisterWithAnotherKeyWarns(t *testing.T) {
	h := newHarness(t, nil)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	register := map[string]string{"id": managerID, "name": "n", "public_key": publicKeyPEM(t, &other.PublicKey)}
	if code, body := h.do("POST", "/api/xtm-composer/register", composerToken, register); code != http.StatusOK {
		t.Fatalf("register: %d %s", code, body)
	}
	if !strings.Contains(h.logs.String(), "secrets won't decrypt") {
		t.Fatalf("missing key mismatch warning, logs: %s", h.logs)
	}
}

func TestDeclaredConnectorIsServedInTheComposerFormat(t *testing.T) {
	h := newHarness(t, nil)
	key, _ := sharedKey(t)

	code, body := h.do("POST", "/control/connectors", controlToken, Definition{
		Name: "Mitre ATT&CK", Image: "openaev/collector-mitre-attack:1.0.0",
		Env:     map[string]string{"COLLECTOR_ID": "abc", "EMPTY": ""},
		Secrets: map[string]string{"OPENAEV_TOKEN": "s3cr3t"},
	})
	if code != http.StatusCreated {
		t.Fatalf("declare: %d %s", code, body)
	}
	v := decodeAs[ConnectorView](t, body)
	if !v.Created {
		t.Fatalf("a new connector should be reported as created: %s", body)
	}
	if v.ContainerName != "mitre-att-ck" || v.RequestedStatus != StatusStarting || v.CurrentStatus != "stopped" {
		t.Fatalf("view %+v", v)
	}
	if strings.Contains(string(body), "s3cr3t") {
		t.Fatalf("the control API leaks the secret: %s", body)
	}

	code, body = h.do("GET", "/api/xtm-composer/manager-1/connector-instances", composerToken, nil)
	if code != http.StatusOK {
		t.Fatalf("instances: %d %s", code, body)
	}
	// Field names as deserialized by ConnectorInstances in xtm-composer
	var raw []map[string]any
	if err := json.Unmarshal(body, &raw); err != nil || len(raw) != 1 {
		t.Fatalf("instances %s: %v", body, err)
	}
	for _, field := range []string{
		"connector_instance_id", "connector_instance_name", "connector_instance_hash", "connector_image",
		"connector_instance_current_status", "connector_instance_requested_status", "connector_instance_configurations",
	} {
		if _, ok := raw[0][field]; !ok {
			t.Errorf("instance has no %s: %s", field, body)
		}
	}
	inst := decodeAs[[]instance](t, body)[0]
	if inst.Image != "openaev/collector-mitre-attack:1.0.0" || inst.Name != "Mitre ATT&CK" || inst.RequestedStatus != "starting" {
		t.Fatalf("instance %+v", inst)
	}
	values := map[string]instanceConfiguration{}
	for _, c := range inst.Configurations {
		values[c.Key] = c
	}
	if c := values["COLLECTOR_ID"]; c.Encrypted || c.Value == nil || *c.Value != "abc" {
		t.Errorf("COLLECTOR_ID = %+v", c)
	}
	if c := values["EMPTY"]; c.Value == nil || *c.Value != "" {
		t.Errorf("EMPTY = %+v", c)
	}
	secret := values["OPENAEV_TOKEN"]
	if !secret.Encrypted || secret.Value == nil || *secret.Value == "s3cr3t" {
		t.Fatalf("OPENAEV_TOKEN not encrypted: %+v", secret)
	}
	if got := decryptLikeComposer(t, key, *secret.Value); got != "s3cr3t" {
		t.Fatalf("decrypted %q", got)
	}

	// Nothing in clear on disk either
	state, _ := os.ReadFile(h.store.path)
	if strings.Contains(string(state), "s3cr3t") {
		t.Fatal("the state file stores the secret in clear")
	}
}

func TestRedeclaringChangesTheHashOnlyWhenTheDefinitionChanges(t *testing.T) {
	h := newHarness(t, nil)
	def := Definition{Name: "c", Image: "img:1", Env: map[string]string{"A": "1"}, Secrets: map[string]string{"S": "x"}}
	_, body := h.do("POST", "/control/connectors", controlToken, def)
	first := decodeAs[ConnectorView](t, body)

	// Same definition: same hash even though the secret ciphertext changes
	code, body := h.do("POST", "/control/connectors", controlToken, def)
	if code != http.StatusOK {
		t.Fatalf("redeclare: got %d, want 200 (update)", code)
	}
	if again := decodeAs[ConnectorView](t, body); again.Hash != first.Hash || again.ID != first.ID || again.Created {
		t.Fatalf("hash or id changed: %+v vs %+v", again, first)
	}
	for _, change := range []Definition{
		{Name: "c", Image: "img:2", Env: def.Env, Secrets: def.Secrets},
		{Name: "c", Image: "img:1", Env: map[string]string{"A": "2"}, Secrets: def.Secrets},
		{Name: "c", Image: "img:1", Env: def.Env, Secrets: map[string]string{"S": "y"}},
	} {
		_, body := h.do("POST", "/control/connectors", controlToken, change)
		if decodeAs[ConnectorView](t, body).Hash == first.Hash {
			t.Errorf("hash unchanged for %+v", change)
		}
	}
}

func TestDeclarationsAreValidated(t *testing.T) {
	h := newHarness(t, nil)
	h.do("POST", "/control/connectors", controlToken, Definition{Name: "My Collector", Image: "img"})
	for _, tc := range []struct {
		def  Definition
		want string
	}{
		{Definition{Image: "img"}, "name is required"},
		{Definition{Name: "x"}, "image is required"},
		{Definition{Name: "my-collector", Image: "img"}, "clashes"},
		{Definition{Name: "x", Image: "img", Env: map[string]string{"K": "1"}, Secrets: map[string]string{"K": "2"}}, "both"},
	} {
		code, body := h.do("POST", "/control/connectors", controlToken, tc.def)
		if code != http.StatusBadRequest || !strings.Contains(string(body), tc.want) {
			t.Errorf("%+v: got %d %s, want 400 %q", tc.def, code, body, tc.want)
		}
	}
}

func TestLifecycleReportedByTheComposer(t *testing.T) {
	h := newHarness(t, nil)
	_, body := h.do("POST", "/control/connectors", controlToken, Definition{Name: "c", Image: "img", Stopped: true})
	c := decodeAs[ConnectorView](t, body)
	if c.RequestedStatus != StatusStopping {
		t.Fatalf("declared stopped: %+v", c)
	}
	base := "/api/xtm-composer/manager-1/connector-instances/" + c.ID

	if code, body := h.do("POST", "/control/connectors/c/start", controlToken, nil); code != http.StatusOK ||
		decodeAs[ConnectorView](t, body).RequestedStatus != StatusStarting {
		t.Fatalf("start: %d %s", code, body)
	}
	code, body := h.do("PUT", base+"/status", composerToken, map[string]string{"connector_instance_current_status": "started"})
	if code != http.StatusOK || decodeAs[instance](t, body).CurrentStatus != "started" {
		t.Fatalf("status: %d %s", code, body)
	}
	code, _ = h.do("PUT", base+"/health-check", composerToken, map[string]any{
		"connector_instance_restart_count": 2, "connector_instance_started_at": "2026-10-28T10:00:00Z",
		"connector_instance_is_in_reboot_loop": false,
	})
	if code != http.StatusOK {
		t.Fatalf("health: %d", code)
	}
	if code, _ := h.do("POST", base+"/logs", composerToken, map[string][]string{"connector_instance_logs": {"a", "b"}}); code != http.StatusOK {
		t.Fatalf("logs: %d", code)
	}
	h.do("POST", base+"/logs", composerToken, map[string][]string{"connector_instance_logs": {"c"}})

	_, body = h.do("GET", "/control/connectors/c", controlToken, nil)
	got := decodeAs[ConnectorView](t, body)
	if got.CurrentStatus != "started" || got.Health == nil || got.Health.RestartCount != 2 || got.LogLines != 3 {
		t.Fatalf("after reports: %+v", got)
	}

	_, body = h.do("GET", "/control/connectors/c/logs", controlToken, nil)
	page := decodeAs[LogsPage](t, body)
	if strings.Join(page.Lines, ",") != "a,b,c" || page.Next != 3 {
		t.Fatalf("logs page %+v", page)
	}
	_, body = h.do("GET", "/control/connectors/c/logs?since=2", controlToken, nil)
	if page := decodeAs[LogsPage](t, body); strings.Join(page.Lines, ",") != "c" || page.Next != 3 {
		t.Fatalf("logs since 2: %+v", page)
	}
	_, body = h.do("GET", "/control/connectors/c/logs?since=10", controlToken, nil)
	if page := decodeAs[LogsPage](t, body); !page.Truncated || len(page.Lines) != 3 {
		t.Fatalf("logs since past the end: %+v", page)
	}

	if code, _ := h.do("POST", "/control/connectors/c/stop", controlToken, nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}
	if code, _ := h.do("DELETE", "/control/connectors/c", controlToken, nil); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	_, body = h.do("GET", "/api/xtm-composer/manager-1/connector-instances", composerToken, nil)
	if strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("instances after delete: %s", body)
	}
	if code, _ := h.do("PUT", base+"/status", composerToken, map[string]string{"connector_instance_current_status": "stopped"}); code != http.StatusNotFound {
		t.Fatalf("status of a removed connector: got %d, want 404", code)
	}
}

func TestLogSnapshotsAreMergedWithoutDuplicates(t *testing.T) {
	store, _ := OpenStore("")
	c, _, _ := store.Upsert(Connector{Name: "c", Image: "i"}, newID)
	for _, push := range []struct {
		snapshot []string
		want     string
	}{
		{[]string{"a", "b"}, "a,b"},
		{[]string{"a", "b"}, "a,b"},          // nothing new
		{[]string{"a", "b", "c"}, "a,b,c"},   // Kubernetes: the whole log again
		{[]string{"b", "c", "d"}, "a,b,c,d"}, // Docker: the last lines
		{[]string{"x", "y"}, "a,b,c,d,x,y"},  // no overlap, e.g. after a restart
		{[]string{"y", "y", "z"}, "a,b,c,d,x,y,y,z"},
		{[]string{"z\n", "end\r\n"}, "a,b,c,d,x,y,y,z,end"}, // Docker log frames end with a newline
	} {
		got, err := store.AppendLogs(c.ID, push.snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(got.Logs, ",") != push.want {
			t.Fatalf("after %v: %v, want %s", push.snapshot, got.Logs, push.want)
		}
	}
}

func TestLogsAreCapped(t *testing.T) {
	store, _ := OpenStore("")
	c, _, _ := store.Upsert(Connector{Name: "c", Image: "i"}, newID)
	lines := make([]string, maxLogLines+5)
	for i := range lines {
		lines[i] = string(rune('a' + i%26))
	}
	got, _ := store.AppendLogs(c.ID, lines)
	if len(got.Logs) != maxLogLines || got.Logs[0] != lines[5] {
		t.Fatalf("kept %d lines starting with %q", len(got.Logs), got.Logs[0])
	}
}

func TestStateSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, _ := OpenStore(path)
	if _, _, err := store.Upsert(Connector{Name: "c", Image: "i", RequestedStatus: StatusStarting}, newID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Register(managerID, "n", "pem"); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if cs := reopened.Connectors(); len(cs) != 1 || cs[0].Name != "c" || reopened.Manager() == nil {
		t.Fatalf("reopened state: %+v %+v", cs, reopened.Manager())
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v, want 0600", info.Mode().Perm())
	}
}

func TestOtherRequestsGoToTheUpstream(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		_, _ = w.Write([]byte("upstream"))
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	h := newHarness(t, u)

	code, body := h.do("POST", "/api/collectors", "collector-token", map[string]string{"a": "b"})
	if code != http.StatusOK || string(body) != "upstream" {
		t.Fatalf("proxied request: %d %s", code, body)
	}
	// The Composer and control APIs stay local
	h.do("GET", "/api/settings/version", "", nil)
	h.do("GET", "/api/xtm-composer/manager-1/connector-instances", composerToken, nil)
	h.do("GET", "/control/status", controlToken, nil)
	if len(seen) != 1 || seen[0] != "POST /api/collectors Bearer collector-token" {
		t.Fatalf("upstream saw %v", seen)
	}
}

func TestWithoutUpstreamOtherRequestsAre404(t *testing.T) {
	h := newHarness(t, nil)
	if code, body := h.do("GET", "/api/collectors", "", nil); code != http.StatusNotFound || !strings.Contains(string(body), "no upstream") {
		t.Fatalf("got %d %s", code, body)
	}
}

func TestContainerNameMatchesTheComposer(t *testing.T) {
	for in, want := range map[string]string{
		"Mitre ATT&CK":       "mitre-att-ck",
		"collector_1":        "collector-1",
		"Already-normalized": "already-normalized",
	} {
		if got := ContainerName(in); got != want {
			t.Errorf("ContainerName(%q) = %q, want %q", in, got, want)
		}
	}
}
