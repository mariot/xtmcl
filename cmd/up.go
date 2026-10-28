package cmd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/mariot/xtmcl/platform"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const (
	defaultKindCluster = "xtm-composer"
	kindNetwork        = "kind"

	// Paths in the Composer image (WORKDIR /opt/xtm-composer, COMPOSER_ENV=production)
	composerKeyPath        = "/opt/xtm-composer/key.pem"
	composerConfigPath     = "/opt/xtm-composer/config/production.yaml"
	composerKubeconfigPath = "/opt/xtm-composer/kubeconfig"

	// Image used to ask Docker where containers reach this machine
	probeImage = "busybox:1.37"
)

var upOpts struct {
	orchestrator string
	image        string
	port         int
	listen       string
	platformHost string
	upstream     string
	network      string
	kindCluster  string
	kubeconfig   string
	composerEnv  []string
	pull         bool
	timeout      time.Duration
}

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Start the local platform and the Composer",
	Long: `Starts a local platform emulating the OpenAEV API, then XTM Composer in a container,
configured to deploy connectors with Docker or on Kubernetes.

Settings are remembered: the next up reuses them unless a flag changes them.

  xtmcl up
  xtmcl up --image filigran/xtm-composer:3.261021.1
  xtmcl up --orchestrator kubernetes          # on a local kind cluster
  xtmcl up --upstream http://localhost:5001   # e.g. Mimikyu or a real OpenAEV
  xtmcl up --composer-env OPENAEV__WITH_PROXY=true --composer-env OPENAEV__HTTP_PROXY=http://proxy:3128`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := initEnvironment()
		if err != nil {
			return err
		}
		if err := applyUpFlags(cmd, e); err != nil {
			return err
		}
		return up(e)
	},
}

func init() {
	rootCmd.AddCommand(upCmd)
	addUpFlags(upCmd)
}

func addUpFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&upOpts.orchestrator, "orchestrator", orchestratorDocker, "where the Composer deploys connectors: docker or kubernetes")
	f.StringVar(&upOpts.image, "image", defaultComposerImage, "XTM Composer image")
	f.IntVar(&upOpts.port, "port", defaultPort, "port of the local platform")
	f.StringVar(&upOpts.listen, "listen", "", "address the local platform listens on (default: detected)")
	f.StringVar(&upOpts.platformHost, "platform-host", "", "address containers use to reach this machine (default: detected)")
	f.StringVar(&upOpts.upstream, "upstream", "", "platform receiving the deployed connectors' own API calls, e.g. Mimikyu (\"none\" to unset)")
	f.StringVar(&upOpts.network, "network", "", "Docker network to attach the connectors to (\"none\" to unset)")
	f.StringVar(&upOpts.kindCluster, "kind-cluster", "", "kind cluster for --orchestrator kubernetes, created if missing (default "+defaultKindCluster+")")
	f.StringVar(&upOpts.kubeconfig, "kubeconfig", "", "kubeconfig of another cluster, instead of kind (must be reachable from a container)")
	f.StringArrayVar(&upOpts.composerEnv, "composer-env", nil, "environment variable KEY=VALUE for the Composer, e.g. OPENAEV__NO_PROXY=... (repeatable, replaces the remembered ones; \"none\" to unset)")
	f.BoolVar(&upOpts.pull, "pull", false, "pull the Composer image before starting it")
	f.DurationVar(&upOpts.timeout, "timeout", 90*time.Second, "how long to wait for the Composer to register")
}

func unsetIfNone(value string) string {
	if value == "none" {
		return ""
	}
	return value
}

func applyUpFlags(cmd *cobra.Command, e *environment) error {
	f := cmd.Flags()
	if f.Changed("orchestrator") {
		e.Orchestrator = upOpts.orchestrator
	}
	if !slices.Contains([]string{orchestratorDocker, orchestratorKubernetes}, e.Orchestrator) {
		return fmt.Errorf("unknown orchestrator %q: use docker or kubernetes", e.Orchestrator)
	}
	if f.Changed("image") {
		e.ComposerImage = upOpts.image
	}
	if f.Changed("port") {
		e.Port = upOpts.port
	}
	if f.Changed("upstream") {
		e.Upstream = unsetIfNone(upOpts.upstream)
	}
	if f.Changed("network") {
		e.Network = unsetIfNone(upOpts.network)
	}
	if f.Changed("kubeconfig") {
		e.Kubeconfig = upOpts.kubeconfig
		e.KindCluster = ""
	}
	if f.Changed("kind-cluster") {
		e.KindCluster = upOpts.kindCluster
		e.Kubeconfig = ""
	}
	if f.Changed("composer-env") {
		env, err := composerEnv(upOpts.composerEnv)
		if err != nil {
			return err
		}
		e.ComposerEnv = env
	}
	if e.Orchestrator == orchestratorKubernetes && e.Kubeconfig == "" && e.KindCluster == "" {
		e.KindCluster = defaultKindCluster
	}
	if e.Orchestrator == orchestratorDocker {
		e.KindCluster, e.Kubeconfig = "", ""
	}
	return nil
}

// composerEnv validates --composer-env values; "none" alone unsets them.
func composerEnv(values []string) ([]string, error) {
	if len(values) == 1 && values[0] == "none" {
		return nil, nil
	}
	env := make([]string, 0, len(values))
	for _, raw := range values {
		key, value, ok := parseAssignment(raw)
		if !ok {
			return nil, fmt.Errorf("invalid --composer-env %q: use KEY=VALUE", raw)
		}
		env = append(env, key+"="+value)
	}
	return env, nil
}

func up(e *environment) error {
	if _, err := runCommand("docker", "version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("docker isn't available: %w", err)
	}

	host := upOpts.platformHost
	if host == "" {
		var err error
		if host, err = detectPlatformHost(); err != nil {
			return err
		}
	}
	e.PlatformHost = host
	e.Listen = upOpts.listen
	if e.Listen == "" {
		e.Listen = listenAddress(host)
	}
	if err := e.save(); err != nil {
		return err
	}

	var kubeconfig []byte
	if e.Orchestrator == orchestratorKubernetes {
		var err error
		if kubeconfig, err = clusterKubeconfig(e); err != nil {
			return err
		}
	}

	// Restart the platform server so that it picks up the settings
	if err := stopServer(e); err != nil {
		return err
	}
	if err := startServer(e); err != nil {
		return err
	}
	fmt.Printf("Platform listening on %s (Composer URL %s)\n", e.controlURL(), e.platformURL())

	startedAt := time.Now()
	if err := startComposer(e, kubeconfig); err != nil {
		return err
	}
	fmt.Printf("Composer %s started (%s, %s)\n", e.composerContainer(), e.ComposerImage, describeOrchestrator(e))
	if err := waitForRegistration(e, startedAt, upOpts.timeout); err != nil {
		return err
	}
	fmt.Println("Composer registered. Next: xtmcl deploy <image> --name <name> --env KEY=VALUE")
	return nil
}

func describeOrchestrator(e *environment) string {
	switch {
	case e.Orchestrator == orchestratorDocker && e.Network != "":
		return "Docker, connectors on network " + e.Network
	case e.Orchestrator == orchestratorDocker:
		return "Docker"
	case e.KindCluster != "":
		return "Kubernetes on kind cluster " + e.KindCluster
	default:
		return "Kubernetes with " + e.Kubeconfig
	}
}

// detectPlatformHost asks Docker for the address of this machine as seen from
// containers (host-gateway): the Docker Desktop or Colima host, or the bridge
// gateway on Linux.
func detectPlatformHost() (string, error) {
	out, err := runCommand("docker", "run", "--rm", "--add-host", "xtm-host:host-gateway", probeImage,
		"sh", "-c", "grep -m1 xtm-host /etc/hosts")
	if err != nil {
		return "", fmt.Errorf("can't find how containers reach this machine (use --platform-host): %w", err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 || net.ParseIP(fields[0]) == nil {
		return "", fmt.Errorf("unexpected host-gateway entry %q (use --platform-host)", out)
	}
	return fields[0], nil
}

// localAddresses returns the IP addresses of this machine; tests replace it.
var localAddresses = func() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		if ipNet, ok := a.(*net.IPNet); ok {
			out = append(out, ipNet.IP.String())
		}
	}
	return out
}

// listenAddress keeps the platform off the network: when containers reach this
// machine on one of its own addresses (Linux bridge), listen there; otherwise
// (Docker Desktop, Colima) they are forwarded to loopback.
func listenAddress(platformHost string) string {
	if slices.Contains(localAddresses(), platformHost) {
		return platformHost
	}
	return "127.0.0.1"
}

// clusterKubeconfig returns the kubeconfig the Composer container uses, creating
// the kind cluster if needed.
func clusterKubeconfig(e *environment) ([]byte, error) {
	if e.Kubeconfig != "" {
		data, err := os.ReadFile(e.Kubeconfig)
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	clusters, err := runCommand("kind", "get", "clusters")
	if err != nil {
		return nil, fmt.Errorf("kind isn't available (install it, or use --kubeconfig): %w", err)
	}
	if !slices.Contains(strings.Fields(clusters), e.KindCluster) {
		fmt.Printf("Creating kind cluster %s (takes a minute)...\n", e.KindCluster)
		if _, err := runCommand("kind", "create", "cluster", "--name", e.KindCluster, "--wait", "3m"); err != nil {
			return nil, err
		}
	}
	// The internal kubeconfig points at the control plane on the kind Docker network
	out, err := runCommand("kind", "get", "kubeconfig", "--internal", "--name", e.KindCluster)
	if err != nil {
		return nil, err
	}
	return []byte(out + "\n"), nil
}

// composerConfig is the Composer configuration (config/production.yaml), on top
// of the image's config/default.yaml.
func composerConfig(e *environment) ([]byte, error) {
	daemon := map[string]any{"selector": e.Orchestrator}
	if e.Orchestrator == orchestratorDocker && e.Network != "" {
		daemon["docker"] = map[string]any{"network_mode": e.Network}
	}
	if e.Orchestrator == orchestratorKubernetes {
		// Lets kind run images loaded with kind load docker-image
		daemon["kubernetes"] = map[string]any{"image_pull_policy": "IfNotPresent"}
	}
	config := map[string]any{
		"manager": map[string]any{
			"id":                       e.ManagerID,
			"name":                     "xtmcl",
			"execute_schedule":         5,
			"ping_alive_schedule":      10,
			"credentials_key_filepath": composerKeyPath,
			"logger": map[string]any{
				// Composer's init_logger swaps the two: directory enables the console
				// and console the file. Both on keeps docker logs working either way.
				"level": "info", "format": "json", "directory": true, "console": true,
			},
		},
		"opencti": map[string]any{"enable": false},
		"openaev": map[string]any{
			"enable":        true,
			"url":           e.platformURL(),
			"token":         e.ComposerToken,
			"logs_schedule": 5,
			"daemon":        daemon,
		},
	}
	return yaml.Marshal(config)
}

// startComposer (re)creates the Composer container and starts it.
func startComposer(e *environment, kubeconfig []byte) error {
	name := e.composerContainer()
	if _, err := runCommand("docker", "rm", "-f", name); err != nil && !strings.Contains(err.Error(), "No such container") {
		return err
	}
	if upOpts.pull {
		fmt.Printf("Pulling %s...\n", e.ComposerImage)
		if _, err := runCommand("docker", "pull", e.ComposerImage); err != nil {
			return err
		}
	}
	args := []string{"create", "--name", name,
		"--label", "xtmcl.manager=" + e.ManagerID,
		"--restart", "unless-stopped",
		"-e", "COMPOSER_ENV=production",
	}
	switch e.Orchestrator {
	case orchestratorDocker:
		args = append(args, "-v", "/var/run/docker.sock:/var/run/docker.sock")
	case orchestratorKubernetes:
		args = append(args, "-e", "KUBECONFIG="+composerKubeconfigPath)
		if e.KindCluster != "" {
			args = append(args, "--network", kindNetwork)
		}
	}
	for _, kv := range e.ComposerEnv {
		args = append(args, "-e", kv)
	}
	args = append(args, e.ComposerImage)
	if _, err := runCommand("docker", args...); err != nil {
		return err
	}

	config, err := composerConfig(e)
	if err != nil {
		return err
	}
	files := map[string][]byte{composerConfigPath: config}
	if kubeconfig != nil {
		files[composerKubeconfigPath] = kubeconfig
	}
	if err := copyIntoContainer(e, name, files); err != nil {
		return err
	}
	if _, err := runCommand("docker", "cp", e.path(keyFile), name+":"+composerKeyPath); err != nil {
		return err
	}
	_, err = runCommand("docker", "start", name)
	return err
}

// copyIntoContainer copies generated files into a created container. They go
// through docker cp rather than bind mounts, which don't work for every path
// with Docker Desktop or Colima.
func copyIntoContainer(e *environment, container string, files map[string][]byte) error {
	tmp, err := os.MkdirTemp(e.dir, "cp-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	for target, data := range files {
		local := tmp + "/" + strings.ReplaceAll(strings.TrimPrefix(target, "/"), "/", "_")
		if err := os.WriteFile(local, data, 0o600); err != nil {
			return err
		}
		if _, err := runCommand("docker", "cp", local, container+":"+target); err != nil {
			return err
		}
	}
	return nil
}

// sleep is replaced by tests.
var sleep = time.Sleep

func waitForRegistration(e *environment, since time.Time, timeout time.Duration) error {
	client := newControlClient(e)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var st platform.Status
		if err := client.do("GET", "/control/status", nil, &st); err == nil &&
			st.Manager != nil && !st.Manager.RegisteredAt.Before(since.Add(-time.Second)) {
			return nil
		}
		state, _ := runCommand("docker", "inspect", "-f", "{{.State.Status}}", e.composerContainer())
		if state == "exited" || state == "dead" {
			logs, _ := runCommand("docker", "logs", "--tail", "20", e.composerContainer())
			return fmt.Errorf("the Composer container stopped:\n%s", logs)
		}
		sleep(time.Second)
	}
	logs, _ := runCommand("docker", "logs", "--tail", "20", e.composerContainer())
	return errors.New("the Composer didn't register in time; its last logs:\n" + logs)
}
