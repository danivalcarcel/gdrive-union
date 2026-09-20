// Command gdunion mounts several Google Drive accounts as one union
// filesystem: each virtual folder can merge children from more than one
// account, but every file lives in exactly one account (no replication -
// losing an account only costs you what was stored there).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	gofuse "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/oauth2"

	"gdriveunion/internal/auth"
	"gdriveunion/internal/config"
	"gdriveunion/internal/gcrypt"
	"gdriveunion/internal/gdrive"
	"gdriveunion/internal/unionfs"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "auth":
		err = authCmd(os.Args[2:])
	case "crypt":
		err = cryptCmd(os.Args[2:])
	case "mount":
		err = mountCmd(os.Args[2:])
	default:
		usage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "gdunion:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  gdunion auth add [--port N] [--bind ADDR] <account-name>   authorize a new Google account
  gdunion auth list                                          list accounts and their quota
  gdunion crypt enable <account-name>                        turn on encryption for an account
  gdunion crypt status                                       show which accounts are encrypted
  gdunion mount <mountpoint>                                 mount the union of all accounts

--port sets a fixed local port (default 53682) for the OAuth callback
instead of a random one, useful for "ssh -L 53682:localhost:53682 host" when
the browser completing the login isn't on the same machine as gdunion.

--bind sets the address that callback server listens on (default
127.0.0.1). Use "0.0.0.0" when running inside a container, so Docker's
published-port forwarding (which doesn't arrive on the loopback interface)
can reach it.

Flags must come before the account name (Go's flag parsing stops at the
first non-flag argument): "gdunion auth add --port 53682 personal", not
"gdunion auth add personal --port 53682".`)
}

func authCmd(args []string) error {
	if len(args) < 1 {
		usage()
		os.Exit(1)
	}
	ctx := context.Background()

	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("auth add", flag.ContinueOnError)
		port := fs.Int("port", 53682, "local port for the OAuth callback (fixed, so ssh -L can forward it)")
		bind := fs.String("bind", "127.0.0.1", `address the OAuth callback server listens on; use "0.0.0.0" when running inside a container so Docker's port forwarding can reach it`)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: gdunion auth add [--port N] [--bind ADDR] <account-name> (flags must come before the account name)")
		}
		name := fs.Arg(0)

		cfg, err := auth.LoadOAuthConfig()
		if err != nil {
			return err
		}
		if _, err := auth.AddAccount(ctx, cfg, name, *bind, *port); err != nil {
			return err
		}
		fmt.Printf("Account %q authorized successfully.\n", name)

		acc, err := loadAccount(ctx, cfg, name)
		if err != nil {
			return fmt.Errorf("preparing app folder: %w", err)
		}
		folder, err := acc.EnsureFolder(ctx, "root", appFolderName(name))
		if err != nil {
			return fmt.Errorf("preparing app folder: %w", err)
		}
		fmt.Printf("gdunion will only use the %q folder in this account's Drive.\n", folder.Name)
		return nil

	case "list":
		names, err := config.ListAccounts()
		if err != nil {
			return err
		}
		if len(names) == 0 {
			fmt.Println("No accounts configured yet. Run: gdunion auth add <name>")
			return nil
		}
		cfg, err := auth.LoadOAuthConfig()
		if err != nil {
			return err
		}
		for _, name := range names {
			acc, err := loadAccount(ctx, cfg, name)
			if err != nil {
				fmt.Printf("%-20s error: %v\n", name, err)
				continue
			}
			q, err := acc.Quota(ctx)
			if err != nil {
				fmt.Printf("%-20s error: %v\n", name, err)
				continue
			}
			limit := "unlimited"
			if q.LimitBytes > 0 {
				limit = humanBytes(q.LimitBytes)
			}
			fmt.Printf("%-20s %s used of %s\n", name, humanBytes(q.UsageBytes), limit)
		}
		return nil

	default:
		return fmt.Errorf("unknown subcommand: %s", args[0])
	}
}

func cryptCmd(args []string) error {
	if len(args) < 1 {
		usage()
		os.Exit(1)
	}

	switch args[0] {
	case "enable":
		if len(args) != 2 {
			return fmt.Errorf("usage: gdunion crypt enable <account-name>")
		}
		name := args[1]
		path, err := config.KeyPath(name)
		if err != nil {
			return err
		}
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("encryption is already enabled for %q (key file exists at %s) - refusing to overwrite it, since that would make everything already encrypted with it unreadable", name, path)
		} else if !os.IsNotExist(err) {
			return err
		}

		key, err := gcrypt.GenerateKey()
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, key[:], 0o600); err != nil {
			return err
		}

		fmt.Printf("Encryption enabled for %q.\n", name)
		fmt.Printf("Key saved to %s - back it up somewhere safe.\n", path)
		fmt.Println("Losing this file means losing access to everything gdunion encrypted with it - there is no recovery.")
		fmt.Println("Only new files/folders created from now on are encrypted; anything already in this account's app folder stays as-is.")
		return nil

	case "status":
		names, err := config.ListAccounts()
		if err != nil {
			return err
		}
		if len(names) == 0 {
			fmt.Println("No accounts configured yet. Run: gdunion auth add <name>")
			return nil
		}
		for _, name := range names {
			path, err := config.KeyPath(name)
			if err != nil {
				return err
			}
			status := "plaintext"
			if _, err := os.Stat(path); err == nil {
				status = "encrypted"
			}
			fmt.Printf("%-20s %s\n", name, status)
		}
		return nil

	default:
		return fmt.Errorf("unknown subcommand: %s", args[0])
	}
}

func mountCmd(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: gdunion mount <mountpoint>")
	}
	mountPoint := args[0]
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return fmt.Errorf("creating mountpoint %s: %w", mountPoint, err)
	}

	ctx := context.Background()
	names, err := config.ListAccounts()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("no accounts configured; run: gdunion auth add <name>")
	}

	cfg, err := auth.LoadOAuthConfig()
	if err != nil {
		return err
	}

	sources := make([]unionfs.Source, 0, len(names))
	for _, name := range names {
		acc, err := loadAccount(ctx, cfg, name)
		if err != nil {
			return fmt.Errorf("loading account %s: %w", name, err)
		}
		folder, err := acc.EnsureFolder(ctx, "root", appFolderName(name))
		if err != nil {
			return fmt.Errorf("preparing app folder for %s: %w", name, err)
		}
		sources = append(sources, unionfs.Source{Account: acc, FileID: folder.ID})
	}

	root := unionfs.NewRoot(sources)
	server, err := gofuse.Mount(mountPoint, root, &gofuse.Options{
		MountOptions: fuse.MountOptions{
			FsName:     "gdunion",
			Name:       "gdunion",
			AllowOther: false,
		},
	})
	if err != nil {
		return fmt.Errorf("mounting at %s: %w", mountPoint, err)
	}

	fmt.Printf("Mounted at %s with %d account(s): %v\n", mountPoint, len(sources), names)
	fmt.Println("Ctrl+C to unmount.")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nUnmounting...")
		if err := server.Unmount(); err != nil {
			fmt.Fprintf(os.Stderr, "gdunion: failed to unmount: %v\n", err)
			fmt.Fprintln(os.Stderr, "Something may still have the mountpoint busy (a shell \"cd\"-ed into it, a file still open). Press Ctrl+C again to force-exit, or from another terminal: fusermount3 -uz "+mountPoint)
			<-sigCh
			os.Exit(1)
		}
	}()

	server.Wait()
	return nil
}

func loadAccount(ctx context.Context, cfg *oauth2.Config, name string) (*gdrive.Account, error) {
	ts, err := auth.TokenSource(ctx, cfg, name)
	if err != nil {
		return nil, err
	}
	acc, err := gdrive.NewAccount(ctx, name, ts)
	if err != nil {
		return nil, err
	}
	cipher, err := loadCipher(name)
	if err != nil {
		return nil, fmt.Errorf("loading encryption key for %s: %w", name, err)
	}
	acc.Cipher = cipher
	return acc, nil
}

// loadCipher returns nil, nil if encryption hasn't been enabled for name
// (no key file yet - see `gdunion crypt enable`).
func loadCipher(name string) (*gcrypt.Cipher, error) {
	path, err := config.KeyPath(name)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(b) != gcrypt.KeySize {
		return nil, fmt.Errorf("key file %s has size %d, expected %d - it may be corrupted", path, len(b), gcrypt.KeySize)
	}
	var key [gcrypt.KeySize]byte
	copy(key[:], b)
	return gcrypt.New(key)
}

// appFolderName is the dedicated Drive folder gdunion confines itself to
// within each account, so mounting never exposes (or writes among) a
// user's pre-existing, unrelated Drive content.
func appFolderName(accountName string) string {
	return "gdrive-union-" + accountName
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
