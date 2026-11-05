package command

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validConfig is a config that passes validation, for cases about one part
// of it.
func validConfig() Config {
	return Config{
		Listen: ":8080",
		OIDC:   OIDCConfig{Issuer: "https://auth.example.com/application/o/campaign-manager/", ClientId: "campaign-manager"},
		Authentik: AuthentikConfig{
			URL:                "https://auth.example.com",
			Token:              "token",
			BaseGroup:          "mapper",
			InvitationFlow:     "campaign-enrollment",
			InvitationLifetime: Duration(168 * time.Hour),
		},
		Wiki:              WikiConfig{URL: "http://dokuwiki.wiki.svc.cluster.local", Token: "token", TemplatesNamespace: "templates"},
		ReconcileInterval: Duration(time.Minute),
	}
}

func TestConfigValidate(t *testing.T) {
	tests := map[string]struct {
		change   func(*Config)
		wantErrs []string
	}{
		"valid": {change: func(*Config) {}},
		"no listen address": {
			change:   func(c *Config) { c.Listen = "" },
			wantErrs: []string{"listen must be set"},
		},
		"an issuer that is not a URL": {
			change:   func(c *Config) { c.OIDC.Issuer = "auth.example.com" },
			wantErrs: []string{`oidc: issuer "auth.example.com" must be an http or https URL`},
		},
		"nothing about authentik": {
			change:   func(c *Config) { c.Authentik = AuthentikConfig{} },
			wantErrs: []string{"authentik: url must be set", "token or token_file must be set", "base_group must be set", "invitation_flow must be set", "invitation_lifetime must be set"},
		},
		"nothing about the wiki": {
			change:   func(c *Config) { c.Wiki = WikiConfig{} },
			wantErrs: []string{"wiki: url must be set", "token or token_file must be set", "templates_namespace must be set"},
		},
		"a token from a file": {
			change: func(c *Config) { c.Wiki.Token, c.Wiki.TokenFile = "", "/secrets/wiki/token" },
		},
		"a token given both ways": {
			change:   func(c *Config) { c.Authentik.TokenFile = "/secrets/authentik/token" },
			wantErrs: []string{"authentik: only one of token and token_file can be set"},
		},
		"every problem is reported": {
			change:   func(c *Config) { *c = Config{} },
			wantErrs: []string{"listen", "oidc:", "authentik:", "wiki:", "reconcile_interval must be set"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			tt.change(&c)
			err := c.Validate()

			if len(tt.wantErrs) == 0 {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want errors mentioning %v", tt.wantErrs)
			}
			for _, want := range tt.wantErrs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Validate() = %q, want it to mention %q", err, want)
				}
			}
		})
	}
}

// TestExampleConfig checks the example config in the repository reads and,
// apart from its placeholder secrets, validates.
func TestExampleConfig(t *testing.T) {
	data, err := os.ReadFile("../../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		t.Fatalf("decoding config.example.json: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("config.example.json does not validate: %v", err)
	}
}

func TestDurationDecode(t *testing.T) {
	tests := map[string]struct {
		json    string
		want    time.Duration
		wantErr bool
	}{
		"minutes":            {json: `"1m"`, want: time.Minute},
		"a week in hours":    {json: `"168h"`, want: 168 * time.Hour},
		"a number":           {json: `60`, wantErr: true},
		"not a length":       {json: `"soon"`, wantErr: true},
		"a number with none": {json: `"60"`, wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var d Duration
			err := json.Unmarshal([]byte(tt.json), &d)
			if tt.wantErr {
				if err == nil {
					t.Errorf("decoding %s = %v, want an error", tt.json, time.Duration(d))
				}
				return
			}
			if err != nil || time.Duration(d) != tt.want {
				t.Errorf("decoding %s = %v, %v; want %v", tt.json, time.Duration(d), err, tt.want)
			}
		})
	}
}

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	tests := map[string]struct {
		token, file string
		want        string
		wantErr     string
	}{
		"from the config":                {token: "abc", want: "abc"},
		"from a file, newline and all":   {file: write("token", "a+b/c=\n"), want: "a+b/c="},
		"from a file that is not there":  {file: filepath.Join(dir, "missing"), wantErr: "reading token"},
		"from a file with nothing in it": {file: write("empty", "\n"), wantErr: "is empty"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := readToken(tt.token, tt.file)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("readToken = %q, %v; want an error mentioning %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("readToken = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}
