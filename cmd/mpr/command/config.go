package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// Config is mpr's config file. It holds nothing secret, so it can be handed
// to another admin; logins are kept apart, in credentials.json beside it.
type Config struct {
	// Server is the campaign-manager server commands talk to when neither
	// --server nor MPR_SERVER is set.
	Server string `json:"server,omitempty"`
}

// configFile returns the config file's path: --config, then MPR_CONFIG, then
// mpr/config.json in the user's config directory.
func (a *app) configFile() (string, error) {
	if a.configPath != "" {
		return a.configPath, nil
	}
	if p := a.getenv("MPR_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding your config directory: %w", err)
	}
	return filepath.Join(dir, "mpr", "config.json"), nil
}

// loadConfig reads the config file at path. A missing file is an empty
// config, since nothing in it is required.
func loadConfig(path string) (*Config, error) {
	var c Config
	if err := readJSONFile(path, &c); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	return &c, nil
}

func (c *Config) save(path string) error {
	if err := writeJSONFile(path, c, 0o644); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	return nil
}

// serverURL returns the server to talk to: --server, then MPR_SERVER, then
// the config file's.
func (a *app) serverURL(c *Config) (string, error) {
	s := a.server
	if s == "" {
		s = a.getenv("MPR_SERVER")
	}
	if s == "" {
		s = c.Server
	}
	if s == "" {
		return "", errors.New(`no server is set: log in with "mpr login --server <url>", or set one with "mpr config set server <url>"`)
	}
	return parseServer(s)
}

// parseServer checks s is a server's URL, and returns it in the form logins
// are saved under, without a trailing slash. Tokens are sent to it, so it
// has to be https, except on this machine.
func parseServer(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("server %q must be a URL, such as https://campaigns.example.com", s)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return "", fmt.Errorf("server %q must be https, since your token is sent to it", s)
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// readJSONFile decodes the file at path into v, refusing unknown fields so
// a misspelled setting is not silently ignored.
func readJSONFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// writeJSONFile replaces the file at path with v, through a temporary file
// in the same directory, so a failed write never leaves half a file. Its
// directory is made private to the user, since logins are kept there.
func writeJSONFile(path string, v any, perm fs.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".mpr-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func newConfigCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show or change mpr's settings",
		Long: `Show or change mpr's settings, kept in mpr/config.json in your config
directory unless --config or MPR_CONFIG says otherwise.

The only setting is server, the campaign-manager server commands talk to.
--server and MPR_SERVER override it for one command.`,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:       "get <setting>",
			Short:     "Print a setting",
			Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
			ValidArgs: []string{"server"},
			RunE: func(cmd *cobra.Command, _ []string) error {
				path, err := a.configFile()
				if err != nil {
					return err
				}
				c, err := loadConfig(path)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), c.Server)
				return err
			},
		},
		&cobra.Command{
			Use:   "set <setting> <value>",
			Short: "Change a setting",
			Args:  cobra.ExactArgs(2),
			ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
				if len(args) == 0 {
					return []string{"server"}, cobra.ShellCompDirectiveNoFileComp
				}
				return nil, cobra.ShellCompDirectiveNoFileComp
			},
			RunE: func(_ *cobra.Command, args []string) error {
				if args[0] != "server" {
					return fmt.Errorf("unknown setting %q; the only setting is server", args[0])
				}
				server, err := parseServer(args[1])
				if err != nil {
					return err
				}
				path, err := a.configFile()
				if err != nil {
					return err
				}
				c, err := loadConfig(path)
				if err != nil {
					return err
				}
				c.Server = server
				return c.save(path)
			},
		},
	)
	return cmd
}
