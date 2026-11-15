// Package cmd is skima's command line: a cobra command per skima command, built fresh by NewRootCmd so a test can run
// a command in-process. Commands return errors - Execute prints them and turns them into the exit code
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// exitInterrupted is the exit code after Ctrl+C (128 + SIGINT), as a shell reports it
const exitInterrupted = 130

// exitError ends a command with an exit code other than 1, or with 1 and no further message - e.g. 2 for
// --detailed-exitcode, or 1 after the problems were already listed
type exitError struct {
	code int
	err  error // printed when set
}

func (e *exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit code %d", e.code)
	}
	return e.err.Error()
}

func (e *exitError) Unwrap() error { return e.err }

// exitCode ends the command with the code and no message
func exitCode(code int) error { return &exitError{code: code} }

// rootOptions are the flags every command has
type rootOptions struct {
	cfgFile string
	output  string
}

// json reports whether the global --output=json was given
func (o *rootOptions) json() bool { return o.output == "json" }

// NewRootCmd builds the skima command with all its subcommands
func NewRootCmd() *cobra.Command {
	opts := &rootOptions{}
	root := &cobra.Command{
		Use:   "skima",
		Short: "Database schema management tool",
		Long: `Tool for managing schema definition, deployment of both
from scratch databases and deltas over time in an existing database.`,
		Version: versionString(),
		// errors are printed once, by Execute - a usage error also says where to get help
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return initConfig(cmd, opts)
		},
	}
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return fmt.Errorf("%w - see %s --help", err, cmd.CommandPath())
	})

	flags := root.PersistentFlags()
	flags.StringVar(&opts.cfgFile, "config", "", "config file (default is $HOME/.skima.yaml)")
	flags.StringVar(&opts.output, "output", "", "The output format - default is plain, human readable texts. For those commands where it makes sense/is documented, you can also have the output in json format")
	flags.StringP("workdir", "w", "", "The manifest working directory containing the state and deltas directories - overrides the config file value")
	flags.String("dbconnstring", "", "The database host - should not include database name, port, schema, username or password - overrides the config file value")
	flags.Int("dbport", 0, "The database port to connect to - overrides the config file value")
	flags.String("dbdb", "", "The database name within the instance to connect to - overrides the config file value")
	flags.String("dbschema", "", "The database schema name to connect to - overrides the config file value")
	flags.String("dbusername", "", "The username to use to log into the database - overrides the config file value")
	flags.String("dbpassword", "", "The database user's password - visible to other users in the process list, so prefer SKM_DBCONN_PASSWORD or the config file")
	flags.String("dbsuperuser", "", "A superuser for apply --bootstrap, which creates the user and schema - set together with --dbsuperuserpwd")
	flags.String("dbsuperuserpwd", "", "The superuser's password - visible in the process list, so prefer SKM_DBCONN_SUPERUSERPWD or the config file")
	root.MarkFlagsRequiredTogether("dbsuperuser", "dbsuperuserpwd")
	for key, flag := range map[string]string{
		"workdir":             "workdir",
		"dbconn.connstring":   "dbconnstring",
		"dbconn.port":         "dbport",
		"dbconn.database":     "dbdb",
		"dbconn.schema":       "dbschema",
		"dbconn.username":     "dbusername",
		"dbconn.password":     "dbpassword",
		"dbconn.superuser":    "dbsuperuser",
		"dbconn.superuserpwd": "dbsuperuserpwd",
	} {
		cobra.CheckErr(viper.BindPFlag(key, flags.Lookup(flag)))
	}

	manifest := newManifestCmd()
	manifest.AddCommand(newListCmd(), newParseCmd(), newValidateCmd(opts), newFmtCmd())
	changeset := newChangesetCmd()
	changeset.AddCommand(newChangesetDiffCmd(opts), newChangesetVerifyCmd(opts), newChangesetBuildCmd(), newChangesetRenameCmd(),
		newChangesetBackfillCmd(), newChangesetHintsCmd())
	root.AddCommand(newApplyCmd(opts), newPlanCmd(opts), newDriftCmd(opts), newInspectCmd(opts), newServeCmd(), manifest, changeset)
	return root
}

// Execute runs skima with the process's arguments and returns the exit code. The command's context is cancelled by
// the first Ctrl+C (or SIGTERM): an apply stops at the statement it is running, rolls back and records the run as
// cancelled. A second Ctrl+C exits at once
func Execute() int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-signals:
			signal.Stop(signals) // a second signal gets the default behaviour - exit now
			fmt.Fprintln(os.Stderr, "\nInterrupted - stopping (press Ctrl+C again to exit now)")
			cancel()
		case <-ctx.Done():
		}
	}()
	return run(ctx, NewRootCmd(), os.Args[1:], os.Stderr)
}

// run executes the command with the arguments, printing its error to errOut, and returns the exit code
func run(ctx context.Context, root *cobra.Command, args []string, errOut io.Writer) int {
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	var exit *exitError
	if errors.As(err, &exit) {
		if exit.err != nil {
			fmt.Fprintln(errOut, "ERROR:", exit.err)
		}
		return exit.code
	}
	fmt.Fprintln(errOut, "ERROR:", err)
	if ctx.Err() != nil {
		return exitInterrupted
	}
	return 1
}

// initConfig reads in config file and ENV variables if set.
func initConfig(cmd *cobra.Command, opts *rootOptions) error {
	if opts.cfgFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(opts.cfgFile)
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		// Search config in home directory with name ".skima" (without extension).
		viper.AddConfigPath(home)
		viper.AddConfigPath(".")
		viper.SetConfigType("yaml")
		viper.SetConfigName(".skima")
	}

	// a password on the command line can be read by any user on the machine (ps, /proc) and lands in shell history
	for flag, env := range map[string]string{"dbpassword": "SKM_DBCONN_PASSWORD", "dbsuperuserpwd": "SKM_DBCONN_SUPERUSERPWD"} {
		if cmd.Flags().Changed(flag) {
			fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: --%s is visible to other users in the process list - set %s or put it in the config file instead\n", flag, env)
		}
	}

	viper.SetEnvPrefix("skm")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv() // read in environment variables that match

	// If a config file is found, read it in. Having none is normal - configuration can come from flags and SKM_
	// environment variables instead. But a config file that was named with --config, or that exists, must be readable -
	// carrying on without it could apply to the wrong database
	if err := viper.ReadInConfig(); err == nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "Using config file:", viper.ConfigFileUsed())
	} else if _, notFound := err.(viper.ConfigFileNotFoundError); !notFound {
		file := opts.cfgFile
		if file == "" {
			file = viper.ConfigFileUsed()
		}
		return fmt.Errorf("unable to read the config file %s: %w", file, err)
	}
	return nil
}
