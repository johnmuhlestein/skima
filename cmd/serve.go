package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"skima/pkg/server"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var servePort int
var serveBind, serveCorsOrigin string

// serveCmd represents the serve command
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serves a local API to read the state files, edit tables, and build changesets from the edits.",
	Long: `Starts a local HTTP API for the state files in the working directory: read every state object, edit tables
(columns, keys, indexes, constraints, triggers, comments) and build the changeset for the edits. Every edit is
validated before it is written, and written in the canonical format - run skima manifest fmt once first, so the first
edit to a hand-formatted file doesn't reformat all of it.

Renaming a column through the API, or adding a NOT NULL column with a backfill, records the intent as a hint (see
skima changeset --help), so the changeset renames or backfills the column. The changes are measured from a git
baseline, as skima changeset diff measures them - --baseline sets the default.

The API is described at /api/v1/openapi.json. It has no authentication, so it binds to 127.0.0.1 and only serves
requests addressed to localhost, 127.0.0.1 or [::1]. Edits need the ETag of the object read (If-Match), so they can't
overwrite a change made since - e.g. by hand.

Example:
  skima serve
  skima serve --port 8080 --baseline origin/main
  curl -s http://127.0.0.1:7012/api/v1/state/tables`,
	Run: func(cmd *cobra.Command, args []string) {
		workdir := viper.GetString("workdir")
		if info, err := os.Stat(workdir); err != nil || !info.IsDir() {
			fmt.Fprintf(os.Stderr, "ERROR: the working directory %q doesn't exist - set it with --workdir or SKM_WORKDIR\n", workdir)
			os.Exit(1)
		}
		if ip := net.ParseIP(serveBind); ip == nil || !ip.IsLoopback() {
			fmt.Fprintf(os.Stderr, "WARNING: binding to %s - the API has no authentication and anyone who can reach it can edit the state files\n", serveBind)
		}
		address := net.JoinHostPort(serveBind, strconv.Itoa(servePort))
		srv := &http.Server{
			Addr:              address,
			Handler:           server.New(workdir, server.Options{BaselineRef: baselineRef, CorsOrigin: serveCorsOrigin}),
			ReadHeaderTimeout: 10 * time.Second,
		}
		listener, err := net.Listen("tcp", address)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
		fmt.Printf("skima serve: http://%s/api/v1 for %s (the API is described at /api/v1/openapi.json) - Ctrl+C to stop\n", listener.Addr(), workdir)

		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-stop
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}()
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
	},
}

func init() {
	rootCmd.AddCommand(serveCmd)
	serveCmd.Flags().IntVar(&servePort, "port", 7012, "The port to listen on")
	serveCmd.Flags().StringVar(&serveBind, "bind", "127.0.0.1", "The address to listen on - the API has no authentication, so keep it on localhost")
	serveCmd.Flags().StringVar(&baselineRef, "baseline", "", "The default git ref changes are measured from - by default the last commit that changed the deltas directory")
	serveCmd.Flags().StringVar(&serveCorsOrigin, "cors-origin", "", "An origin allowed to call the API from a browser, e.g. a UI dev server at http://localhost:5173")
}
