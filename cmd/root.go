package cmd

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version is set at release time (-ldflags "-X github.com/mariot/xtmcl/cmd.version=...").
var version = ""

var rootCmd = &cobra.Command{
	Use:   "xtmcl",
	Short: "XTM Composer Launchpad: drive XTM Composer without OpenAEV or OpenCTI",
	Long: `xtmcl (XTM Composer Launchpad) starts XTM Composer against a local platform that
emulates the OpenAEV API, so you can deploy, start, stop and remove connectors,
collectors and injectors on Docker or Kubernetes from the command line.

  xtmcl up                    # start the local platform and the Composer
  xtmcl deploy <image> ...    # declare a connector: the Composer deploys it
  xtmcl ps                    # connectors, as reported by the Composer
  xtmcl logs <name> -f        # connector logs, as reported by the Composer
  xtmcl down                  # stop everything`,
	Version:       buildVersion(),
	SilenceUsage:  true,
	SilenceErrors: true,
}

// buildVersion is the release version, else the module version of a go install, else "dev".
func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

// Execute is the entrypoint called by main.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
