// Package command is mpr's command line: logging in to a campaign-manager
// server, and managing campaigns through it.
package command

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"github.com/spf13/cobra"
)

// app is what mpr's commands share: the flags every command takes, and how
// to reach the environment and the network, which tests replace.
type app struct {
	configPath string
	server     string
	getenv     func(string) string
	hc         *http.Client
}

// NewRootCmd returns mpr's root command, with every command under it.
func NewRootCmd() *cobra.Command {
	return newRootCmd(&app{
		getenv: os.Getenv,
		hc:     &http.Client{Timeout: 30 * time.Second},
	})
}

func newRootCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mpr",
		Short: "Manage campaigns, and who is in them",
		Long: `mpr manages campaigns through a campaign-manager server: creating them, and
adding and inviting people into them.

Start by logging in, which also makes the server your default:

    mpr login --server https://campaigns.example.com`,
		Version: version(),
		// An answer from the server is not a mistake in how mpr was run, so
		// errors are printed without the usage after them.
		SilenceUsage: true,
	}
	f := cmd.PersistentFlags()
	f.StringVar(&a.configPath, "config", "", "config file (default mpr/config.json in your config directory, or $MPR_CONFIG)")
	f.StringVar(&a.server, "server", "", "campaign-manager server URL (default from the config file, or $MPR_SERVER)")

	cmd.AddCommand(
		newLoginCmd(a),
		newLogoutCmd(a),
		newWhoamiCmd(a),
		newCampaignCmd(a),
		newInviteCmd(a),
		newConfigCmd(a),
	)
	return cmd
}

// version is mpr's module version. Go stamps it into every build from the
// tag or commit built, so releases need no flags to set it.
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}

// addJSONFlag gives cmd a --json flag, for scripts to read its result from.
func addJSONFlag(cmd *cobra.Command, v *bool) {
	cmd.Flags().BoolVar(v, "json", false, "print the result as JSON")
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
