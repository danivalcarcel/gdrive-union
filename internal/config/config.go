// Package config locates gdunion's on-disk state: the shared OAuth client
// credentials and the per-account token files.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Dir returns the gdunion config directory, creating it if needed.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "gdunion")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// ClientSecretPath is where the Google OAuth "Desktop app" client credentials
// (downloaded from Google Cloud Console) must be placed.
func ClientSecretPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "client_secret.json"), nil
}

// accountsDir holds one <name>.json token file per added Google account.
func accountsDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	accDir := filepath.Join(dir, "accounts")
	if err := os.MkdirAll(accDir, 0o700); err != nil {
		return "", err
	}
	return accDir, nil
}

func TokenPath(name string) (string, error) {
	if strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("invalid account name %q", name)
	}
	dir, err := accountsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

// KeyPath is where an account's encryption master key lives, if it has one
// (see internal/gcrypt). Its presence is what turns encryption on for that
// account - there's no separate on/off setting.
func KeyPath(name string) (string, error) {
	if strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("invalid account name %q", name)
	}
	dir, err := accountsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".key"), nil
}

// ListAccounts returns the names of all accounts that have a saved token,
// sorted alphabetically (this order also decides collision-naming priority
// in the union tree: earlier accounts keep the plain name).
func ListAccounts() ([]string, error) {
	dir, err := accountsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	return names, nil
}

// PolicyPath stores the write-placement policy setting (used once write
// support lands); kept here now so the file layout doesn't shift later.
func PolicyPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "policy.json"), nil
}

func ReadJSON(path string, v interface{}) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func WriteJSON(path string, v interface{}) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
