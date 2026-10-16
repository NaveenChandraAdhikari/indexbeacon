package cmd

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/indexbeacon/indexbeacon/internal/config"
	"github.com/indexbeacon/indexbeacon/internal/server"
	"github.com/indexbeacon/indexbeacon/web"
)

var serveOpts struct {
	Listen      string
	Config      string
	Open        bool
	AllowRemote bool
	Token       string
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the web interface",
	Long: `serve starts a local web interface for watching databases and running
analyses.

Add as many databases as you like, watch sequential-scan pressure as it
happens, and run an analysis when something looks wrong. Everything the
interface does is read-only, exactly like the command line: it will never
create, drop or alter anything.

By default it listens on the loopback address only, because the
configuration holds database connection strings and the interface has no
user accounts.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runServe(cmd.Context())
	},
}

func init() {
	f := serveCmd.Flags()
	f.StringVar(&serveOpts.Listen, "listen", "127.0.0.1:8777", "address to listen on")
	f.StringVar(&serveOpts.Config, "config", "", "configuration file (default: ~/.config/idxa/config.json)")
	f.BoolVar(&serveOpts.Open, "open", false, "open a browser once the server is ready")
	f.BoolVar(&serveOpts.AllowRemote, "allow-remote", false,
		"allow binding a non-loopback address, which requires an access token")
	f.StringVar(&serveOpts.Token, "token", os.Getenv("IDXA_TOKEN"),
		"access token to require, or IDXA_TOKEN (generated when --allow-remote is used and neither is set)")
	rootCmd.AddCommand(serveCmd)
}

func runServe(ctx context.Context) error {
	path := serveOpts.Config
	if path == "" {
		def, err := config.DefaultPath()
		if err != nil {
			return fmt.Errorf("could not determine a configuration path: %w", err)
		}
		path = def
	}

	store, err := config.Open(path)
	if err != nil {
		return err
	}

	assets, err := fs.Sub(web.Assets, "dist")
	if err != nil {
		return fmt.Errorf("the web interface was not built into this binary: %w", err)
	}

	srv, err := server.New(server.Options{
		Addr:        serveOpts.Listen,
		AllowRemote: serveOpts.AllowRemote,
		Token:       serveOpts.Token,
		Store:       store,
		Assets:      assets,
		Version:     version,
		Logger:      log.New(os.Stderr, "", log.LstdFlags),
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	url := srv.URL()
	fmt.Fprintf(os.Stderr, "idxa is running at %s\n", url)
	fmt.Fprintf(os.Stderr, "configuration: %s\n", store.Path())
	if srv.Token() != "" {
		fmt.Fprintf(os.Stderr,
			"this address is reachable from the network, so an access token is required.\n"+
				"Anyone with the link above can read every database you configure here.\n")
	}
	fmt.Fprintf(os.Stderr, "read-only: idxa never creates, drops or alters anything.\n")

	if serveOpts.Open {
		openBrowser(url)
	}
	return srv.ListenAndServe(ctx)
}

// openBrowser makes a best effort to open the interface, ignoring failures
// because a missing browser is not a reason for the server to stop.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
