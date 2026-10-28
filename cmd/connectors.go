package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mariot/xtmcl/platform"
	"github.com/spf13/cobra"
)

var output io.Writer = os.Stdout

// outf and outln print to the command output.
func outf(format string, a ...any) { _, _ = fmt.Fprintf(output, format, a...) }

func outln(a ...any) { _, _ = fmt.Fprintln(output, a...) }

var deployOpts struct {
	name     string
	env      []string
	envFiles []string
	secrets  []string
	stopped  bool
	wait     bool
	timeout  time.Duration
}

var deployCmd = &cobra.Command{
	Use:   "deploy <image>",
	Short: "Declare a connector: the Composer deploys it",
	Long: `Declares a connector (or updates the one with the same name). The Composer deploys
it on its next cycle, and redeploys it whenever its image or settings change.

The Composer adds OPENAEV_URL, pointing at the local platform: with up --upstream,
the connector's own API calls are forwarded to that platform.

  xtmcl deploy openaev/collector-mitre-attack:rolling --name mitre \
    --env COLLECTOR_ID=$(uuidgen) --secret OPENAEV_TOKEN=$TOKEN
  xtmcl deploy busybox:1.37 --name hello --env-file hello.env`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := loadEnvironment()
		if err != nil {
			return err
		}
		def, err := buildDefinition(args[0], deployOpts.name, deployOpts.envFiles, deployOpts.env, deployOpts.secrets)
		if err != nil {
			return err
		}
		def.Stopped = deployOpts.stopped
		client := newControlClient(e)
		var v platform.ConnectorView
		if err := client.do("POST", "/control/connectors", def, &v); err != nil {
			return err
		}
		action := "updated"
		if v.Created {
			action = "declared"
		}
		outf("Connector %s %s (%s, deployed as %s)\n", v.Name, action, v.Image, v.ContainerName)
		if !deployOpts.wait || deployOpts.stopped {
			return nil
		}
		return waitForStatus(client, v.Name, "started", deployOpts.timeout)
	},
}

var psJSON bool

var psCmd = &cobra.Command{
	Use:     "ps",
	Aliases: []string{"ls"},
	Short:   "List the connectors and their status, as reported by the Composer",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := loadEnvironment()
		if err != nil {
			return err
		}
		var connectors []platform.ConnectorView
		if err := newControlClient(e).do("GET", "/control/connectors", nil, &connectors); err != nil {
			return err
		}
		if psJSON {
			enc := json.NewEncoder(output)
			enc.SetIndent("", "  ")
			return enc.Encode(connectors)
		}
		printConnectors(connectors)
		return nil
	},
}

func lifecycleCommand(use, short, method, suffix, done string) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <name>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := loadEnvironment()
			if err != nil {
				return err
			}
			var v platform.ConnectorView
			if err := newControlClient(e).do(method, connectorPath(args[0], suffix), nil, &v); err != nil {
				return err
			}
			outf("Connector %s %s\n", v.Name, done)
			return nil
		},
	}
}

var logsFollow bool

var logsCmd = &cobra.Command{
	Use:   "logs <name>",
	Short: "Show the connector logs reported by the Composer",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := loadEnvironment()
		if err != nil {
			return err
		}
		return printLogs(newControlClient(e), args[0], logsFollow)
	},
}

func init() {
	rootCmd.AddCommand(deployCmd, psCmd, logsCmd,
		lifecycleCommand("start", "Ask the Composer to start a connector", "POST", "/start", "will be started"),
		lifecycleCommand("stop", "Ask the Composer to stop a connector", "POST", "/stop", "will be stopped"),
		lifecycleCommand("rm", "Remove a connector: the Composer removes its deployment", "DELETE", "", "removed"),
	)
	f := deployCmd.Flags()
	f.StringVar(&deployOpts.name, "name", "", "connector name (default: from the image)")
	f.StringArrayVarP(&deployOpts.env, "env", "e", nil, "setting KEY=VALUE (repeatable)")
	f.StringArrayVar(&deployOpts.envFiles, "env-file", nil, "file of KEY=VALUE settings (repeatable)")
	f.StringArrayVar(&deployOpts.secrets, "secret", nil, "secret setting KEY=VALUE, sent encrypted (repeatable); KEY alone reads it from your environment")
	f.BoolVar(&deployOpts.stopped, "stopped", false, "deploy without starting it")
	f.BoolVar(&deployOpts.wait, "wait", true, "wait until the Composer reports it started")
	f.DurationVar(&deployOpts.timeout, "timeout", 3*time.Minute, "how long to wait")
	psCmd.Flags().BoolVar(&psJSON, "json", false, "print JSON")
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "keep printing new logs")
}

// defaultName derives a connector name from an image: registry/org/name:tag -> name.
func defaultName(image string) string {
	name := image
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.IndexAny(name, ":@"); i >= 0 {
		name = name[:i]
	}
	return name
}

func parseAssignment(raw string) (string, string, bool) {
	key, value, ok := strings.Cut(raw, "=")
	key = strings.TrimSpace(key)
	return key, value, ok && key != ""
}

// readEnvFile reads KEY=VALUE lines, ignoring blank lines and # comments.
func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := parseAssignment(strings.TrimPrefix(line, "export "))
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		values[key] = unquote(value)
	}
	return values, scanner.Err()
}

func unquote(value string) string {
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return value[1 : len(value)-1]
	}
	return value
}

// buildDefinition assembles the connector settings: env files, then --env, then --secret.
func buildDefinition(image, name string, envFiles, env, secrets []string) (platform.Definition, error) {
	def := platform.Definition{Name: name, Image: image, Env: map[string]string{}, Secrets: map[string]string{}}
	if def.Name == "" {
		def.Name = defaultName(image)
	}
	for _, path := range envFiles {
		values, err := readEnvFile(path)
		if err != nil {
			return def, err
		}
		for k, v := range values {
			def.Env[k] = v
		}
	}
	for _, raw := range env {
		key, value, ok := parseAssignment(raw)
		if !ok {
			return def, fmt.Errorf("--env %q: expected KEY=VALUE", raw)
		}
		def.Env[key] = value
	}
	for _, raw := range secrets {
		key, value, ok := parseAssignment(raw)
		if !ok {
			// KEY alone: read the value from the environment, keeping it out of the shell history
			key = strings.TrimSpace(raw)
			var found bool
			if value, found = os.LookupEnv(key); !found || key == "" {
				return def, fmt.Errorf("--secret %q: expected KEY=VALUE, or KEY set in your environment", raw)
			}
		}
		delete(def.Env, key)
		def.Secrets[key] = value
	}
	return def, nil
}

func printConnectors(connectors []platform.ConnectorView) {
	if len(connectors) == 0 {
		outln("No connectors. Declare one with xtmcl deploy <image>")
		return
	}
	w := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tIMAGE\tREQUESTED\tSTATUS\tRESTARTS\tSTARTED AT\tDEPLOYED AS")
	for _, c := range connectors {
		restarts, started := "-", "-"
		if c.Health != nil {
			restarts = fmt.Sprint(c.Health.RestartCount)
			if c.Health.RebootLoop {
				restarts += " (reboot loop)"
			}
			started = c.Health.StartedAt
		}
		requested := c.RequestedStatus
		switch requested {
		case platform.StatusStarting:
			requested = "start"
		case platform.StatusStopping:
			requested = "stop"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.Name, c.Image, requested, c.CurrentStatus, restarts, started, c.ContainerName)
	}
	_ = w.Flush()
}

func waitForStatus(client *controlClient, name, want string, timeout time.Duration) error {
	outf("Waiting for the Composer to report %s %s...\n", name, want)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var v platform.ConnectorView
		if err := client.do("GET", connectorPath(name, ""), nil, &v); err != nil {
			return err
		}
		if v.CurrentStatus == want {
			outf("Connector %s %s\n", name, want)
			return nil
		}
		sleep(2 * time.Second)
	}
	return fmt.Errorf("connector %s not %s after %s: check xtmcl logs %s and xtmcl status", name, want, timeout, name)
}

func printLogs(client *controlClient, name string, follow bool) error {
	since := 0
	for {
		var page platform.LogsPage
		if err := client.do("GET", connectorPath(name, fmt.Sprintf("/logs?since=%d", since)), nil, &page); err != nil {
			return err
		}
		if page.Truncated && since > 0 {
			outln("[... older lines dropped ...]")
		}
		for _, line := range page.Lines {
			outln(line)
		}
		since = page.Next
		if !follow {
			return nil
		}
		sleep(2 * time.Second)
	}
}
