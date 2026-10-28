package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mariot/xtmcl/platform"
)

// Orchestrators the Composer can be started with.
const (
	orchestratorDocker     = "docker"
	orchestratorKubernetes = "kubernetes"
)

// environment is the persisted setup of the local platform and its Composer.
type environment struct {
	ManagerID     string `json:"manager_id"`
	ComposerToken string `json:"composer_token"`
	ControlToken  string `json:"control_token"`

	Port          int    `json:"port"`
	Listen        string `json:"listen"`
	PlatformHost  string `json:"platform_host"`
	Orchestrator  string `json:"orchestrator"`
	ComposerImage string `json:"composer_image"`
	Upstream      string `json:"upstream,omitempty"`
	Network       string `json:"network,omitempty"`
	KindCluster   string `json:"kind_cluster,omitempty"`
	Kubeconfig    string `json:"kubeconfig,omitempty"`
	// ComposerEnv is extra KEY=VALUE environment for the Composer container,
	// e.g. OPENAEV__HTTP_PROXY; it overrides the generated configuration.
	ComposerEnv []string `json:"composer_env,omitempty"`

	dir string
}

const (
	envFile       = "env.json"
	keyFile       = "key.pem"
	stateFile     = "state.json"
	serverLogFile = "server.log"
	pidFile       = "server.pid"

	defaultPort          = 47380
	defaultComposerImage = "filigran/xtm-composer:latest"
	platformVersion      = "xtmcl"
)

// homeDir is where the environment lives: $XTMCL_HOME, else
// $XDG_STATE_HOME/xtmcl, else ~/.local/state/xtmcl.
func homeDir() (string, error) {
	if dir := os.Getenv("XTMCL_HOME"); dir != "" {
		return dir, nil
	}
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "xtmcl"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "xtmcl"), nil
}

func (e *environment) path(name string) string { return filepath.Join(e.dir, name) }

// loadEnvironment reads the environment, or returns errNoEnvironment before the first up.
func loadEnvironment() (*environment, error) {
	dir, err := homeDir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, envFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoEnvironment
	}
	if err != nil {
		return nil, err
	}
	e := &environment{dir: dir}
	if err := json.Unmarshal(data, e); err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Join(dir, envFile), err)
	}
	return e, nil
}

var errNoEnvironment = errors.New("no environment yet: run xtmcl up first")

// initEnvironment loads the environment, or creates it with a new manager ID,
// tokens and credentials key.
func initEnvironment() (*environment, error) {
	e, err := loadEnvironment()
	if err == nil {
		return e, nil
	}
	if !errors.Is(err, errNoEnvironment) {
		return nil, err
	}
	dir, err := homeDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	e = &environment{
		dir:           dir,
		ManagerID:     platform.NewUUID(),
		ComposerToken: platform.NewToken(),
		ControlToken:  platform.NewToken(),
		Port:          defaultPort,
		Orchestrator:  orchestratorDocker,
		ComposerImage: defaultComposerImage,
	}
	fmt.Println("Generating the Composer credentials key (RSA 4096)...")
	if err := platform.GenerateKey(e.path(keyFile)); err != nil {
		return nil, err
	}
	return e, e.save()
}

func (e *environment) save() error {
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(e.path(envFile), data, 0o600)
}

// platformURL is the platform address for the Composer and the connectors it deploys.
func (e *environment) platformURL() string {
	return fmt.Sprintf("http://%s:%d", e.PlatformHost, e.Port)
}

// controlURL is the platform address for the CLI.
func (e *environment) controlURL() string {
	return fmt.Sprintf("http://%s:%d", e.Listen, e.Port)
}

// composerContainer is the name of the Composer container.
func (e *environment) composerContainer() string {
	return "xtmcl-" + e.ManagerID[:8]
}

// kubeContext is the kubectl context of the cluster the Composer deploys to.
func (e *environment) kubeContext() string {
	if e.KindCluster != "" {
		return "kind-" + e.KindCluster
	}
	return ""
}
