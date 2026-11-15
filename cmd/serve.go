package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/johnmuhlestein/skima/internal/server"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func newServeCmd() *cobra.Command {
	var port int
	var bind, baselineRef, corsOrigin string
	cmd := &cobra.Command{
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
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			workdir := viper.GetString("workdir")
			if info, err := os.Stat(workdir); err != nil || !info.IsDir() {
				return fmt.Errorf("the working directory %q doesn't exist - set it with --workdir or SKM_WORKDIR", workdir)
			}
			if ip := net.ParseIP(bind); ip == nil || !ip.IsLoopback() {
				fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: binding to %s - the API has no authentication and anyone who can reach it can edit the state files\n", bind)
			}
			address := net.JoinHostPort(bind, strconv.Itoa(port))
			srv := &http.Server{
				Addr:              address,
				Handler:           server.New(workdir, server.Options{BaselineRef: baselineRef, CorsOrigin: corsOrigin}),
				ReadHeaderTimeout: 10 * time.Second,
			}
			listener, err := net.Listen("tcp", address)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "skima serve: http://%s/api/v1 for %s (the API is described at /api/v1/openapi.json) - Ctrl+C to stop\n", listener.Addr(), workdir)

			// Ctrl+C cancels the command's context: finish the requests in flight, then stop
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				<-cmd.Context().Done()
				ctx, cancel := context.WithTimeout(context.WithoutCancel(cmd.Context()), 10*time.Second)
				defer cancel()
				_ = srv.Shutdown(ctx)
			}()
			if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			<-stopped
			return nil
		},
	}
	cmd.Flags().IntVar(&port, "port", 7012, "The port to listen on")
	cmd.Flags().StringVar(&bind, "bind", "127.0.0.1", "The address to listen on - the API has no authentication, so keep it on localhost")
	cmd.Flags().StringVar(&baselineRef, "baseline", "", "The default git ref changes are measured from - by default the last commit that changed the deltas directory")
	cmd.Flags().StringVar(&corsOrigin, "cors-origin", "", "An origin allowed to call the API from a browser, e.g. a UI dev server at http://localhost:5173")
	return cmd
}
