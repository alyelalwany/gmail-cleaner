// Package auth handles the OAuth 3-legged flow for a personal Gmail account
// and caches the resulting refresh token on disk. Later runs (including
// non-interactive cron/serverless) reuse the cached token; the oauth2 library
// transparently refreshes the short-lived access token as needed.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// Scopes granted at OAuth time:
//   - MailGoogleComScope: full mailbox access (read, modify, permanent delete).
//   - GmailSettingsBasicScope: manage Gmail settings including filters.
//     Not implied by mail.google.com/ — filter endpoints require it explicitly.
var Scopes = []string{
	gmail.MailGoogleComScope,
	gmail.GmailSettingsBasicScope,
}

// DefaultConfigDir returns ~/.config/gmail-cleaner.
func DefaultConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gmail-cleaner"), nil
}

func credentialsPath(dir string) string { return filepath.Join(dir, "credentials.json") }
func tokenPath(dir string) string       { return filepath.Join(dir, "token.json") }

func loadOAuthConfig(dir string) (*oauth2.Config, error) {
	raw, err := os.ReadFile(credentialsPath(dir))
	if err != nil {
		return nil, fmt.Errorf("read credentials.json in %s: %w (download OAuth Desktop client from Google Cloud Console)", dir, err)
	}
	cfg, err := google.ConfigFromJSON(raw, Scopes...)
	if err != nil {
		return nil, fmt.Errorf("parse credentials.json: %w", err)
	}
	return cfg, nil
}

func loadToken(dir string) (*oauth2.Token, error) {
	f, err := os.Open(tokenPath(dir))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var t oauth2.Token
	if err := json.NewDecoder(f).Decode(&t); err != nil {
		return nil, err
	}
	return &t, nil
}

func saveToken(dir string, tok *oauth2.Token) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(tokenPath(dir), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(tok)
}

// Login runs the interactive OAuth flow via a loopback redirect and writes
// the resulting token to disk. Safe to re-run; will overwrite the existing
// token.
func Login(ctx context.Context, dir string) error {
	cfg, err := loadOAuthConfig(dir)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("open loopback listener: %w", err)
	}
	defer listener.Close()
	redirectURL := fmt.Sprintf("http://%s/callback", listener.Addr().String())
	cfg.RedirectURL = redirectURL

	state, err := randomState()
	if err != nil {
		return err
	}
	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)

	fmt.Println("Open this URL in your browser to authorize:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	fmt.Println("Waiting for Google to redirect back to " + redirectURL + " ...")

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if e := q.Get("error"); e != "" {
				fmt.Fprintf(w, "Authorization failed: %s\n", e)
				errCh <- fmt.Errorf("oauth error from Google: %s", e)
				return
			}
			if got := q.Get("state"); got != state {
				http.Error(w, "state mismatch", http.StatusBadRequest)
				errCh <- fmt.Errorf("state mismatch")
				return
			}
			code := q.Get("code")
			if code == "" {
				http.Error(w, "missing code", http.StatusBadRequest)
				errCh <- fmt.Errorf("missing authorization code in callback")
				return
			}
			fmt.Fprintln(w, "Authorization complete. You can close this tab and return to the terminal.")
			codeCh <- code
		}),
	}
	go func() { _ = srv.Serve(listener) }()
	defer srv.Shutdown(context.Background())

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}

	tok, err := cfg.Exchange(ctx, code, oauth2.SetAuthURLParam("redirect_uri", redirectURL))
	if err != nil {
		return fmt.Errorf("exchange code for token: %w", err)
	}
	if tok.RefreshToken == "" {
		return fmt.Errorf("no refresh token returned by Google; revoke the app at https://myaccount.google.com/permissions and re-run auth")
	}
	if err := saveToken(dir, tok); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	fmt.Println("Token saved to", tokenPath(dir))
	return nil
}

// NewService returns a Gmail client authenticated with the cached token.
// The underlying HTTP client refreshes the access token automatically.
func NewService(ctx context.Context, dir string) (*gmail.Service, error) {
	cfg, err := loadOAuthConfig(dir)
	if err != nil {
		return nil, err
	}
	tok, err := loadToken(dir)
	if err != nil {
		return nil, fmt.Errorf("load token: %w (run `gmail-cleaner auth` first)", err)
	}
	client := cfg.Client(ctx, tok)
	return gmail.NewService(ctx, option.WithHTTPClient(client))
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
