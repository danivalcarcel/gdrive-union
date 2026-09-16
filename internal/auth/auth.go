// Package auth handles the Google OAuth2 dance for each Drive account and
// keeps refreshed tokens persisted to disk so gdunion only ever asks for a
// browser login once per account.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"gdriveunion/internal/config"
)

// DriveScope grants full read/write access, needed since gdunion supports
// creating, editing, moving and trashing files. Accounts authorized under
// the old read-only scope must be re-added (gdunion auth add <name> again)
// after upgrading, since Google won't silently widen an existing token.
const DriveScope = "https://www.googleapis.com/auth/drive"

// LoadOAuthConfig reads the shared "Desktop app" OAuth client credentials
// downloaded from Google Cloud Console (see README for how to create one).
func LoadOAuthConfig() (*oauth2.Config, error) {
	path, err := config.ClientSecretPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w (see README: create an OAuth client and save it there)", path, err)
	}
	cfg, err := google.ConfigFromJSON(b, DriveScope)
	if err != nil {
		return nil, fmt.Errorf("parsing client secret: %w", err)
	}
	return cfg, nil
}

// AddAccount runs an interactive OAuth authorization for a new Google
// account, using a loopback HTTP server to receive the redirect, and saves
// the resulting token under the given account name. port == 0 picks a free
// port at random; pass a fixed port when the browser completing the login
// isn't on the same machine as gdunion (e.g. running on a headless server
// over SSH), so an `ssh -L <port>:localhost:<port>` tunnel can reach it.
func AddAccount(ctx context.Context, cfg *oauth2.Config, name string, port int) (*oauth2.Token, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("starting local listener on port %d: %w", port, err)
	}
	defer listener.Close()

	actualPort := listener.Addr().(*net.TCPAddr).Port
	redirectURL := fmt.Sprintf("http://127.0.0.1:%d/callback", actualPort)

	// Work on a copy so concurrent AddAccount calls (unlikely, but cheap to
	// support) don't race on RedirectURL.
	localCfg := *cfg
	localCfg.RedirectURL = redirectURL

	state, err := randomState()
	if err != nil {
		return nil, err
	}

	authURL := localCfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("oauth state mismatch")
			return
		}
		if errMsg := r.URL.Query().Get("error"); errMsg != "" {
			http.Error(w, errMsg, http.StatusBadRequest)
			errCh <- fmt.Errorf("google returned error: %s", errMsg)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			errCh <- fmt.Errorf("no code in callback")
			return
		}
		fmt.Fprintln(w, "Authorization complete, you can close this tab now.")
		codeCh <- code
	})
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	defer server.Shutdown(context.Background())

	fmt.Printf("Open this URL to authorize account %q (or it'll open in your browser automatically):\n%s\n", name, authURL)
	tryOpenBrowser(authURL)

	select {
	case code := <-codeCh:
		tok, err := localCfg.Exchange(ctx, code)
		if err != nil {
			return nil, fmt.Errorf("exchanging code: %w", err)
		}
		if err := saveToken(name, tok); err != nil {
			return nil, err
		}
		return tok, nil
	case err := <-errCh:
		return nil, err
	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("timed out waiting for authorization")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func tryOpenBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start() // best effort; the printed URL is the fallback
}

func saveToken(name string, tok *oauth2.Token) error {
	path, err := config.TokenPath(name)
	if err != nil {
		return err
	}
	return config.WriteJSON(path, tok)
}

func loadToken(name string) (*oauth2.Token, error) {
	path, err := config.TokenPath(name)
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := config.ReadJSON(path, &tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

// persistingTokenSource wraps an oauth2.TokenSource and writes the token
// back to disk whenever it changes (i.e. whenever it was refreshed), so the
// next run reuses the new refresh/access token instead of re-authenticating.
type persistingTokenSource struct {
	name   string
	last   string // last-seen access token, to detect refreshes cheaply
	source oauth2.TokenSource
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.source.Token()
	if err != nil {
		return nil, err
	}
	if tok.AccessToken != p.last {
		p.last = tok.AccessToken
		if err := saveToken(p.name, tok); err != nil {
			return tok, fmt.Errorf("persisting refreshed token for %s: %w", p.name, err)
		}
	}
	return tok, nil
}

// TokenSource returns a self-refreshing, self-persisting token source for an
// already-added account.
func TokenSource(ctx context.Context, cfg *oauth2.Config, name string) (oauth2.TokenSource, error) {
	tok, err := loadToken(name)
	if err != nil {
		return nil, fmt.Errorf("loading token for %s: %w (run: gdunion auth add %s)", name, err, name)
	}
	base := cfg.TokenSource(ctx, tok)
	return oauth2.ReuseTokenSource(tok, &persistingTokenSource{name: name, last: tok.AccessToken, source: base}), nil
}
