package command

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"

	"golang.org/x/oauth2"
)

// credentials are the logins mpr holds, by server URL. Each server has its
// own, so pointing one command at another server does not end the first
// login.
type credentials map[string]*savedLogin

// savedLogin is one server's login: its tokens, and the authentik provider
// they came from, so a refresh needs nothing from the server.
type savedLogin struct {
	Issuer   string        `json:"issuer"`
	ClientId string        `json:"client_id"`
	TokenURL string        `json:"token_url"`
	Token    *oauth2.Token `json:"token"`
}

// credentialsFile is where the logins are kept: beside the config file, so a
// --config of its own keeps its own logins.
func credentialsFile(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "credentials.json")
}

// loadCredentials reads the logins at path. A missing file is no logins.
func loadCredentials(path string) (credentials, error) {
	creds := credentials{}
	if err := readJSONFile(path, &creds); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("reading logins: %w", err)
	}
	return creds, nil
}

// save writes the logins readable by the user alone, since anyone who can
// read the refresh tokens can act as them.
func (c credentials) save(path string) error {
	if err := writeJSONFile(path, c, 0o600); err != nil {
		return fmt.Errorf("saving logins: %w", err)
	}
	return nil
}

// savingSource saves every new token src hands out. authentik can replace
// the refresh token when the access token is refreshed, and revokes the old
// one, so a refreshed token that is not saved ends the login.
type savingSource struct {
	src  oauth2.TokenSource
	save func(*oauth2.Token) error

	mu   sync.Mutex
	last string
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	t, err := s.src.Token()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.AccessToken != s.last {
		if err := s.save(t); err != nil {
			return nil, err
		}
		s.last = t.AccessToken
	}
	return t, nil
}
