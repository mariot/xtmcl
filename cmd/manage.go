package cmd

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/mariot/xtmcl/platform"
	"github.com/spf13/cobra"
)

var downOpts struct {
	keepConnectors bool
	purge          bool
	deleteCluster  bool
}

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Stop the Composer, its connectors and the local platform",
	Long: `Stops the Composer and the local platform, and removes the connector deployments.
The declared connectors are kept: the next up deploys them again.

  xtmcl down
  xtmcl down --keep-connectors   # leave the deployments running
  xtmcl down --purge             # also forget the connectors and credentials`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := loadEnvironment()
		if err != nil {
			return err
		}
		return down(e)
	},
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the local platform, the Composer and its registration",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := loadEnvironment()
		if err != nil {
			return err
		}
		return status(e)
	},
}

var composerLogsFollow bool

var composerLogsCmd = &cobra.Command{
	Use:   "composer-logs",
	Short: "Show the logs of the Composer itself",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := loadEnvironment()
		if err != nil {
			return err
		}
		dockerArgs := []string{"logs", "--tail", "200"}
		if composerLogsFollow {
			dockerArgs = append(dockerArgs, "-f")
		}
		docker := exec.Command("docker", append(dockerArgs, e.composerContainer())...)
		docker.Stdout, docker.Stderr = os.Stdout, os.Stderr
		return docker.Run()
	},
}

func init() {
	rootCmd.AddCommand(downCmd, statusCmd, composerLogsCmd)
	f := downCmd.Flags()
	f.BoolVar(&downOpts.keepConnectors, "keep-connectors", false, "leave the connector deployments running")
	f.BoolVar(&downOpts.purge, "purge", false, "also forget the declared connectors, the credentials key and the settings")
	f.BoolVar(&downOpts.deleteCluster, "delete-cluster", false, "also delete the kind cluster")
	composerLogsCmd.Flags().BoolVarP(&composerLogsFollow, "follow", "f", false, "keep printing new logs")
}

func notFound(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "No such container") || strings.Contains(err.Error(), "not found"))
}

func down(e *environment) error {
	var errs []error
	// The Composer first, so it doesn't redeploy what is removed next
	if _, err := runCommand("docker", "rm", "-f", e.composerContainer()); err != nil && !notFound(err) {
		errs = append(errs, err)
	} else {
		outf("Composer %s removed\n", e.composerContainer())
	}
	if !downOpts.keepConnectors {
		if err := removeDeployments(e); err != nil {
			errs = append(errs, err)
		}
	}
	if err := stopServer(e); err != nil {
		errs = append(errs, err)
	} else {
		outln("Platform stopped")
	}
	if downOpts.deleteCluster && e.KindCluster != "" {
		if _, err := runCommand("kind", "delete", "cluster", "--name", e.KindCluster); err != nil {
			errs = append(errs, err)
		} else {
			outf("kind cluster %s deleted\n", e.KindCluster)
		}
	}
	if downOpts.purge {
		if err := os.RemoveAll(e.dir); err != nil {
			errs = append(errs, err)
		} else {
			outf("Removed %s\n", e.dir)
		}
	}
	return errors.Join(errs...)
}

// removeDeployments removes what the Composer deployed, found by its manager label.
func removeDeployments(e *environment) error {
	selector := "opencti-manager=" + e.ManagerID
	switch e.Orchestrator {
	case orchestratorKubernetes:
		args := kubectlArgs(e, "delete", "deployments", "-l", selector, "--ignore-not-found", "--wait=false")
		out, err := runCommand("kubectl", args...)
		if err != nil {
			return err
		}
		if out != "" {
			outln(out)
		}
	default:
		ids, err := runCommand("docker", "ps", "-aq", "--filter", "label="+selector)
		if err != nil {
			return err
		}
		if ids == "" {
			return nil
		}
		if _, err := runCommand("docker", append([]string{"rm", "-f"}, strings.Fields(ids)...)...); err != nil {
			return err
		}
		outf("Removed %d connector container(s)\n", len(strings.Fields(ids)))
	}
	return nil
}

func kubectlArgs(e *environment, args ...string) []string {
	if e.Kubeconfig != "" {
		return append([]string{"--kubeconfig", e.Kubeconfig}, args...)
	}
	return append([]string{"--context", e.kubeContext()}, args...)
}

func status(e *environment) error {
	outf("State:         %s\n", e.dir)
	outf("Orchestrator:  %s\n", describeOrchestrator(e))
	outf("Platform:      %s (Composer URL %s)\n", e.controlURL(), e.platformURL())
	if e.Upstream != "" {
		outf("Upstream:      %s\n", e.Upstream)
	}
	state, err := runCommand("docker", "inspect", "-f", "{{.State.Status}} since {{.State.StartedAt}}", e.composerContainer())
	if err != nil {
		state = "not running"
	}
	outf("Composer:      %s, %s (%s)\n", e.composerContainer(), state, e.ComposerImage)

	var st platform.Status
	if err := newControlClient(e).do("GET", "/control/status", nil, &st); err != nil {
		outf("Platform API:  not reachable (%v)\n", err)
		return nil
	}
	if st.Manager == nil {
		outln("Registration:  the Composer hasn't registered yet")
	} else {
		outf("Registration:  manager %s registered %s, last ping %s ago\n",
			st.Manager.ID, st.Manager.RegisteredAt.Format(time.RFC3339), time.Since(st.Manager.LastPingAt).Round(time.Second))
	}
	outf("Connectors:    %d (xtmcl ps)\n", st.Connectors)
	return nil
}
