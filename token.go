package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// storedToken is the on-disk representation of the OAuth2 token state.
// It lives in its own file, separate from the user-edited config, because
// this file is rewritten by the program on every token rotation.
type storedToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
}

// loadToken reads the token file. A missing file is not an error: it means we
// still have to bootstrap from the refresh token in the config.
func loadToken(path string) (*oauth2.Token, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read token file %q: %w", path, err)
	}

	var st storedToken
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("cannot parse token file %q: %w", path, err)
	}
	if st.RefreshToken == "" {
		return nil, fmt.Errorf("token file %q contains no refresh_token", path)
	}

	return &oauth2.Token{
		AccessToken:  st.AccessToken,
		RefreshToken: st.RefreshToken,
		Expiry:       st.Expiry,
	}, nil
}

// saveToken writes the token atomically: temp file in the same directory,
// fsync, rename. On a Raspberry Pi an SD card write can be interrupted by a
// power loss at any moment, and a half-written token file would mean a manual
// re-authorization at dev.netatmo.com.
func saveToken(path string, token *oauth2.Token) (err error) {
	st := storedToken{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		Expiry:       token.Expiry,
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".netatmo-token-*.tmp")
	if err != nil {
		return fmt.Errorf("cannot create temp file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	if err = tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("cannot rename token file into place: %w", err)
	}

	// fsync the directory so the rename itself survives a power loss.
	if d, derr := os.Open(dir); derr == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// persistentTokenSource wraps an oauth2.TokenSource and writes every newly
// issued token to disk. Netatmo rotates the refresh token on each call to the
// token endpoint and invalidates the previous one, so a token we fail to
// persist is a token we can never use again.
type persistentTokenSource struct {
	src  oauth2.TokenSource
	path string

	mu   sync.Mutex
	seen string // access token of the last persisted state
}

func newPersistentTokenSource(cfg *oauth2.Config, seed *oauth2.Token, path string) oauth2.TokenSource {
	return &persistentTokenSource{
		src:  oauth2.ReuseTokenSource(seed, cfg.TokenSource(context.Background(), seed)),
		path: path,
		seen: seed.AccessToken,
	}
}

func (p *persistentTokenSource) Token() (*oauth2.Token, error) {
	token, err := p.src.Token()
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if token.AccessToken == p.seen {
		// Served from the in-memory cache, nothing was rotated.
		return token, nil
	}

	if err := saveToken(p.path, token); err != nil {
		// Deliberately non-fatal: the refresh already happened and the old
		// token is dead either way, so let this run succeed. But shout about
		// it, because the next run will have to re-authorize.
		log.Printf("WARNING: could not persist rotated token to %s: %v", p.path, err)
		log.Printf("WARNING: the next start will likely fail with invalid_grant")
		return token, nil
	}

	p.seen = token.AccessToken
	log.Printf("token refreshed, valid until %s", token.Expiry.Local().Format(time.RFC3339))
	return token, nil
}
