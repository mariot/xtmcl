package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mariot/xtmcl/platform"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

// fakeRunner records the external commands and answers them from a script.
type fakeRunner struct {
	calls   []string
	answers map[string]string // command prefix -> output
	fails   map[string]error  // command prefix -> error
}

func (f *fakeRunner) run(name string, args ...string) (string, error) {
	call := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, call)
	for prefix, err := range f.fails {
		if strings.HasPrefix(call, prefix) {
			return "", err
		}
	}
	for prefix, out := range f.answers {
		if strings.HasPrefix(call, prefix) {
			return out, nil
		}
	}
	return "", nil
}

func (f *fakeRunner) called(prefix string) []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func stubRunner(t *testing.T) *fakeRunner {
	t.Helper()
	f := &fakeRunner{answers: map[string]string{}, fails: map[string]error{}}
	previous := runCommand
	runCommand = f.run
	t.Cleanup(func() { runCommand = previous })
	return f
}

func testEnvironment(t *testing.T) *environment {
	t.Helper()
	return &environment{
		dir:           t.TempDir(),
		ManagerID:     "0123abcd-0000-4000-8000-000000000000",
		ComposerToken: "composer-token",
		ControlToken:  "control-token",
		Port:          defaultPort,
		Listen:        "127.0.0.1",
		PlatformHost:  "192.168.5.2",
		Orchestrator:  orchestratorDocker,
		ComposerImage: "filigran/xtm-composer:test",
	}
}

func TestDefaultName(t *testing.T) {
	for image, want := range map[string]string{
		"openaev/collector-mitre-attack:rolling": "collector-mitre-attack",
		"ghcr.io/org/team/connector:1.2":         "connector",
		"localhost:5000/collector-x":             "collector-x",
		"busybox":                                "busybox",
		"img@sha256:abc":                         "img",
	} {
		if got := defaultName(image); got != want {
			t.Errorf("defaultName(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestReadEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.env")
	_ = os.WriteFile(path, []byte("# comment\n\nA=1\nexport B=\"two words\"\nC='x=y'\nEMPTY=\n"), 0o600)
	values, err := readEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "B": "two words", "C": "x=y", "EMPTY": ""}
	if fmt.Sprint(values) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", values, want)
	}
	_ = os.WriteFile(path, []byte("A=1\nnot an assignment\n"), 0o600)
	if _, err := readEnvFile(path); err == nil || !strings.Contains(err.Error(), ":2:") {
		t.Fatalf("expected a line 2 error, got %v", err)
	}
}

func TestBuildDefinition(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "x.env")
	_ = os.WriteFile(envFile, []byte("A=from-file\nB=from-file\nTOKEN=in-clear-in-file\n"), 0o600)
	t.Setenv("MY_SECRET", "from-env")

	def, err := buildDefinition("org/collector-x:1", "", []string{envFile},
		[]string{"B=from-flag", "C=with=equals"}, []string{"TOKEN=s3cr3t", "MY_SECRET"})
	if err != nil {
		t.Fatal(err)
	}
	if def.Name != "collector-x" || def.Image != "org/collector-x:1" {
		t.Fatalf("name/image: %+v", def)
	}
	wantEnv := map[string]string{"A": "from-file", "B": "from-flag", "C": "with=equals"}
	if fmt.Sprint(def.Env) != fmt.Sprint(wantEnv) {
		t.Fatalf("env %v, want %v (a secret must not stay in clear)", def.Env, wantEnv)
	}
	wantSecrets := map[string]string{"TOKEN": "s3cr3t", "MY_SECRET": "from-env"}
	if fmt.Sprint(def.Secrets) != fmt.Sprint(wantSecrets) {
		t.Fatalf("secrets %v, want %v", def.Secrets, wantSecrets)
	}

	for _, bad := range [][]string{{"--env", "NOVALUE"}, {"--env", "=x"}, {"--secret", "UNSET_IN_ENV_XYZ"}} {
		env, secrets := []string{}, []string{}
		if bad[0] == "--env" {
			env = append(env, bad[1])
		} else {
			secrets = append(secrets, bad[1])
		}
		if _, err := buildDefinition("img", "n", nil, env, secrets); err == nil {
			t.Errorf("%v: expected an error", bad)
		}
	}
}

func TestComposerConfig(t *testing.T) {
	e := testEnvironment(t)
	e.Network = "mimikyu_default"
	data, err := composerConfig(e)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Manager struct {
			ID                     string `yaml:"id"`
			CredentialsKeyFilepath string `yaml:"credentials_key_filepath"`
		} `yaml:"manager"`
		OpenCTI struct {
			Enable bool `yaml:"enable"`
		} `yaml:"opencti"`
		OpenAEV struct {
			Enable bool   `yaml:"enable"`
			URL    string `yaml:"url"`
			Token  string `yaml:"token"`
			Daemon struct {
				Selector string            `yaml:"selector"`
				Docker   map[string]string `yaml:"docker"`
				Kube     map[string]string `yaml:"kubernetes"`
			} `yaml:"daemon"`
		} `yaml:"openaev"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	if config.Manager.ID != e.ManagerID || config.Manager.CredentialsKeyFilepath != composerKeyPath {
		t.Errorf("manager: %+v", config.Manager)
	}
	if config.OpenCTI.Enable || !config.OpenAEV.Enable || config.OpenAEV.URL != "http://192.168.5.2:47380" ||
		config.OpenAEV.Token != "composer-token" {
		t.Errorf("platforms: %+v %+v", config.OpenCTI, config.OpenAEV)
	}
	if config.OpenAEV.Daemon.Selector != "docker" || config.OpenAEV.Daemon.Docker["network_mode"] != "mimikyu_default" {
		t.Errorf("docker daemon: %+v", config.OpenAEV.Daemon)
	}

	e.Orchestrator, e.Network = orchestratorKubernetes, ""
	data, _ = composerConfig(e)
	_ = yaml.Unmarshal(data, &config)
	if config.OpenAEV.Daemon.Selector != "kubernetes" || config.OpenAEV.Daemon.Kube["image_pull_policy"] != "IfNotPresent" {
		t.Errorf("kubernetes daemon: %+v", config.OpenAEV.Daemon)
	}
}

func TestStartComposerWithDocker(t *testing.T) {
	f := stubRunner(t)
	e := testEnvironment(t)
	if err := platform.GenerateKey(e.path(keyFile)); err != nil {
		t.Fatal(err)
	}
	if err := startComposer(e, nil); err != nil {
		t.Fatal(err)
	}
	name := e.composerContainer()
	creates := f.called("docker create")
	if len(creates) != 1 {
		t.Fatalf("docker create calls: %v", f.calls)
	}
	for _, want := range []string{
		"--name " + name, "--label xtmcl.manager=" + e.ManagerID, "--restart unless-stopped",
		"-e COMPOSER_ENV=production", "-v /var/run/docker.sock:/var/run/docker.sock",
	} {
		if !strings.Contains(creates[0], want) {
			t.Errorf("docker create misses %q: %s", want, creates[0])
		}
	}
	if !strings.HasSuffix(creates[0], " filigran/xtm-composer:test") || strings.Contains(creates[0], "--network") {
		t.Errorf("docker create: %s", creates[0])
	}
	copies := strings.Join(f.called("docker cp"), "\n")
	for _, target := range []string{name + ":" + composerConfigPath, e.path(keyFile) + " " + name + ":" + composerKeyPath} {
		if !strings.Contains(copies, target) {
			t.Errorf("missing docker cp to %s in:\n%s", target, copies)
		}
	}
	if strings.Contains(copies, composerKubeconfigPath) {
		t.Errorf("no kubeconfig in Docker mode:\n%s", copies)
	}
	// Remove the old container first, start the new one last
	if !strings.HasPrefix(f.calls[0], "docker rm -f "+name) || f.calls[len(f.calls)-1] != "docker start "+name {
		t.Errorf("order: %v", f.calls)
	}
}

func TestStartComposerOnKind(t *testing.T) {
	f := stubRunner(t)
	e := testEnvironment(t)
	e.Orchestrator, e.KindCluster = orchestratorKubernetes, "xtm"
	_ = platform.GenerateKey(e.path(keyFile))
	if err := startComposer(e, []byte("kubeconfig")); err != nil {
		t.Fatal(err)
	}
	create := f.called("docker create")[0]
	if !strings.Contains(create, "--network kind") || !strings.Contains(create, "-e KUBECONFIG="+composerKubeconfigPath) ||
		strings.Contains(create, "docker.sock") {
		t.Errorf("docker create: %s", create)
	}
	if !strings.Contains(strings.Join(f.called("docker cp"), "\n"), ":"+composerKubeconfigPath) {
		t.Errorf("kubeconfig not copied: %v", f.calls)
	}
}

func TestClusterKubeconfigCreatesTheKindClusterOnlyWhenMissing(t *testing.T) {
	f := stubRunner(t)
	e := testEnvironment(t)
	e.Orchestrator, e.KindCluster = orchestratorKubernetes, "xtm"
	f.answers["kind get clusters"] = "other"
	f.answers["kind get kubeconfig --internal --name xtm"] = "apiVersion: v1"
	data, err := clusterKubeconfig(e)
	if err != nil || string(data) != "apiVersion: v1\n" {
		t.Fatalf("kubeconfig %q, %v", data, err)
	}
	if len(f.called("kind create cluster --name xtm")) != 1 {
		t.Fatalf("cluster not created: %v", f.calls)
	}

	f.calls = nil
	f.answers["kind get clusters"] = "other\nxtm"
	if _, err := clusterKubeconfig(e); err != nil {
		t.Fatal(err)
	}
	if len(f.called("kind create")) != 0 {
		t.Fatalf("existing cluster recreated: %v", f.calls)
	}
}

func TestDetectPlatformHost(t *testing.T) {
	f := stubRunner(t)
	f.answers["docker run --rm --add-host xtm-host:host-gateway"] = "192.168.5.2\txtm-host"
	if host, err := detectPlatformHost(); err != nil || host != "192.168.5.2" {
		t.Fatalf("got %q, %v", host, err)
	}
	f.answers["docker run --rm --add-host xtm-host:host-gateway"] = "garbage"
	if _, err := detectPlatformHost(); err == nil || !strings.Contains(err.Error(), "--platform-host") {
		t.Fatalf("expected a hint to --platform-host, got %v", err)
	}
}

func TestListenAddressStaysOffTheNetwork(t *testing.T) {
	previous := localAddresses
	t.Cleanup(func() { localAddresses = previous })
	localAddresses = func() []string { return []string{"127.0.0.1", "172.17.0.1", "10.0.0.12"} }

	// Linux: the bridge gateway is a local address, listen there only
	if got := listenAddress("172.17.0.1"); got != "172.17.0.1" {
		t.Errorf("Linux bridge: got %q", got)
	}
	// Docker Desktop, Colima: forwarded to loopback
	if got := listenAddress("192.168.5.2"); got != "127.0.0.1" {
		t.Errorf("VM-based Docker: got %q", got)
	}
}

func newUpCommand(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "up"}
	addUpFlags(cmd)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestUpFlagsOverrideOnlyWhatTheyChange(t *testing.T) {
	e := testEnvironment(t)
	e.Upstream, e.Network = "http://mimikyu:5000", "net"

	if err := applyUpFlags(newUpCommand(t), e); err != nil {
		t.Fatal(err)
	}
	if e.ComposerImage != "filigran/xtm-composer:test" || e.Upstream != "http://mimikyu:5000" || e.Network != "net" {
		t.Fatalf("persisted settings lost: %+v", e)
	}

	if err := applyUpFlags(newUpCommand(t, "--upstream", "none", "--network", "none", "--image", "img:2"), e); err != nil {
		t.Fatal(err)
	}
	if e.Upstream != "" || e.Network != "" || e.ComposerImage != "img:2" {
		t.Fatalf("flags not applied: %+v", e)
	}

	if err := applyUpFlags(newUpCommand(t, "--orchestrator", "kubernetes"), e); err != nil {
		t.Fatal(err)
	}
	if e.Orchestrator != orchestratorKubernetes || e.KindCluster != defaultKindCluster {
		t.Fatalf("kubernetes defaults: %+v", e)
	}
	if err := applyUpFlags(newUpCommand(t, "--kubeconfig", "/k"), e); err != nil || e.KindCluster != "" || e.Kubeconfig != "/k" {
		t.Fatalf("kubeconfig replaces kind: %+v %v", e, err)
	}
	if err := applyUpFlags(newUpCommand(t, "--orchestrator", "docker"), e); err != nil || e.Kubeconfig != "" || e.KindCluster != "" {
		t.Fatalf("docker clears the cluster settings: %+v %v", e, err)
	}
	if err := applyUpFlags(newUpCommand(t, "--orchestrator", "swarm"), e); err == nil {
		t.Fatal("unknown orchestrator accepted")
	}
}

func TestWaitForRegistrationReportsAStoppedComposer(t *testing.T) {
	f := stubRunner(t)
	previous := sleep
	sleep = func(time.Duration) {}
	t.Cleanup(func() { sleep = previous })
	e := testEnvironment(t)
	e.Port = 1 // nothing listens: the platform never answers
	f.answers["docker inspect"] = "exited"
	f.answers["docker logs"] = "panicked at 'Missing kubernetes configuration'"
	err := waitForRegistration(e, time.Now(), time.Minute)
	if err == nil || !strings.Contains(err.Error(), "stopped") || !strings.Contains(err.Error(), "Missing kubernetes configuration") {
		t.Fatalf("got %v", err)
	}
}

func TestRemoveDeployments(t *testing.T) {
	f := stubRunner(t)
	e := testEnvironment(t)
	f.answers["docker ps -aq --filter label=opencti-manager="+e.ManagerID] = "aaa\nbbb"
	if err := removeDeployments(e); err != nil {
		t.Fatal(err)
	}
	if got := f.called("docker rm -f"); len(got) != 1 || got[0] != "docker rm -f aaa bbb" {
		t.Fatalf("docker rm: %v", f.calls)
	}

	f.calls = nil
	e.Orchestrator, e.KindCluster = orchestratorKubernetes, "xtm"
	if err := removeDeployments(e); err != nil {
		t.Fatal(err)
	}
	want := "kubectl --context kind-xtm delete deployments -l opencti-manager=" + e.ManagerID + " --ignore-not-found --wait=false"
	if !slices.Contains(f.calls, want) {
		t.Fatalf("kubectl: %v, want %s", f.calls, want)
	}
}

func TestDownKeepsGoingAfterAnError(t *testing.T) {
	f := stubRunner(t)
	e := testEnvironment(t)
	downOpts.keepConnectors, downOpts.purge, downOpts.deleteCluster = false, false, false
	f.fails["docker rm -f "+e.composerContainer()] = errors.New("daemon error")
	f.answers["docker ps -aq"] = "aaa"
	err := down(e)
	if err == nil || !strings.Contains(err.Error(), "daemon error") {
		t.Fatalf("got %v", err)
	}
	if len(f.called("docker rm -f aaa")) != 1 {
		t.Fatalf("connector containers not removed after the error: %v", f.calls)
	}
}

// --- CLI flow against a real platform server ---

var keyOnce sync.Once
var sharedKeyPath string

func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	// Cobra keeps flag values between executions: reset them
	var reset func(c *cobra.Command)
	reset = func(c *cobra.Command) {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				_ = sv.Replace(nil)
			} else {
				_ = f.Value.Set(f.DefValue)
			}
			f.Changed = false
		})
		for _, sub := range c.Commands() {
			reset(sub)
		}
	}
	reset(rootCmd)
	buf := &bytes.Buffer{}
	previous := output
	output = buf
	defer func() { output = previous }()
	rootCmd.SetArgs(args)
	err := rootCmd.Execute()
	return buf.String(), err
}

func startTestPlatform(t *testing.T) (*environment, *platform.Store) {
	t.Helper()
	keyOnce.Do(func() {
		dir, _ := os.MkdirTemp("", "xtmcl-cmd")
		sharedKeyPath = filepath.Join(dir, keyFile)
		if err := platform.GenerateKey(sharedKeyPath); err != nil {
			t.Fatal(err)
		}
	})
	pub, err := platform.LoadPublicKey(sharedKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	e := testEnvironment(t)
	store, _ := platform.OpenStore(e.path(stateFile))
	srv := httptest.NewServer(platform.NewServer(store, platform.Options{
		Version: platformVersion, ManagerID: e.ManagerID, ComposerToken: e.ComposerToken,
		ControlToken: e.ControlToken, PublicKey: pub, Logger: log.New(&bytes.Buffer{}, "", 0),
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	e.Listen = u.Hostname()
	e.Port, _ = strconv.Atoi(u.Port())
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XTMCL_HOME", e.dir)
	return e, store
}

func TestCLIFlow(t *testing.T) {
	_, store := startTestPlatform(t)

	out, err := runCLI(t, "deploy", "openaev/collector-x:1", "--env", "A=1", "--secret", "TOKEN=s3cr3t", "--wait=false")
	if err != nil || !strings.Contains(out, "Connector collector-x declared") {
		t.Fatalf("deploy: %q %v", out, err)
	}
	out, err = runCLI(t, "deploy", "openaev/collector-x:2", "--name", "collector-x", "--wait=false")
	if err != nil || !strings.Contains(out, "Connector collector-x updated") {
		t.Fatalf("redeploy: %q %v", out, err)
	}
	if c, _ := store.Get("collector-x"); c.Image != "openaev/collector-x:2" || len(c.Settings) != 0 {
		t.Fatalf("redeploy replaces the definition: %+v", c)
	}

	// The Composer reports the connector started
	c, _ := store.Get("collector-x")
	_, _ = store.SetCurrentStatus(c.ID, "started")
	_, _ = store.SetHealth(c.ID, platform.Health{RestartCount: 4, StartedAt: "2026-10-28T10:00:00Z", RebootLoop: true})
	_, _ = store.AppendLogs(c.ID, []string{"line 1\n", "line 2\n"})

	out, _ = runCLI(t, "ps")
	for _, want := range []string{"NAME", "collector-x", "openaev/collector-x:2", "start", "started", "4 (reboot loop)"} {
		if !strings.Contains(out, want) {
			t.Errorf("ps misses %q:\n%s", want, out)
		}
	}
	if out, _ := runCLI(t, "logs", "collector-x"); out != "line 1\nline 2\n" {
		t.Errorf("logs: %q", out)
	}
	if out, err := runCLI(t, "stop", "collector-x"); err != nil || !strings.Contains(out, "will be stopped") {
		t.Fatalf("stop: %q %v", out, err)
	}
	if c, _ := store.Get("collector-x"); c.RequestedStatus != platform.StatusStopping {
		t.Fatalf("requested %s", c.RequestedStatus)
	}
	if _, err := runCLI(t, "start", "unknown"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("start unknown: %v", err)
	}
	if out, err := runCLI(t, "rm", "collector-x"); err != nil || !strings.Contains(out, "removed") {
		t.Fatalf("rm: %q %v", out, err)
	}
	if out, _ := runCLI(t, "ps"); !strings.Contains(out, "No connectors") {
		t.Fatalf("ps after rm: %q", out)
	}
}

func TestDeployWaitsUntilStarted(t *testing.T) {
	_, store := startTestPlatform(t)
	previous := sleep
	t.Cleanup(func() { sleep = previous })
	polls := 0
	sleep = func(time.Duration) {
		// The Composer reports the connector started after a few cycles
		if polls++; polls == 3 {
			c, _ := store.Get("hello")
			_, _ = store.SetCurrentStatus(c.ID, "started")
		}
	}
	out, err := runCLI(t, "deploy", "busybox:1.37", "--name", "hello")
	if err != nil || !strings.Contains(out, "Connector hello started") {
		t.Fatalf("deploy --wait: %q %v", out, err)
	}
}

func TestCommandsBeforeUpExplainWhatToDo(t *testing.T) {
	t.Setenv("XTMCL_HOME", t.TempDir())
	if _, err := runCLI(t, "ps"); !errors.Is(err, errNoEnvironment) {
		t.Fatalf("got %v", err)
	}
}
