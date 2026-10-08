package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var cfgFile string
var output string

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "skima",
	Short: "Database schema management tool",
	Long: `Tool for managing schema definition, deployment of both
from scratch databases and deltas over time in an existing database.`,
	// Uncomment the following line if your bare application
	// has an action associated with it:
	// Run: func(cmd *cobra.Command, args []string) { },
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	// Persistent Flags and bindings
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.skima.yaml)")
	rootCmd.PersistentFlags().StringVar(&output, "output", "", "The output format - default is plain, human readable texts. For those commands where it makes sense/is documented, you can also have the output in json format")
	rootCmd.PersistentFlags().StringP("workdir", "w", "", "The manifest working directory containing the state and deltas directories - overrides the config file value")
	viper.BindPFlag("workdir", rootCmd.PersistentFlags().Lookup("workdir"))
	rootCmd.PersistentFlags().String("dbconnstring", "", "The connection string for the database instance - should not include database name, port, schema, username or pwd -  - overrides the config file value")
	rootCmd.PersistentFlags().Int("dbport", 0, "The database port to connect to - overrides the config file value")
	rootCmd.PersistentFlags().String("dbdb", "", "The database name within the instance to connect to - overrides the config file value")
	rootCmd.PersistentFlags().String("dbschema", "", "The database schema name to connect to - overrides the config file value")
	rootCmd.PersistentFlags().String("dbusername", "", "The username to use to log into the database - overrides the config file value")
	rootCmd.PersistentFlags().String("dbpassword", "", "To pass in a database password from the command line")
	rootCmd.PersistentFlags().String("dbsuperuser", "", "The superuser for the database - unlike the standard user, this user can work across all schemas including the public schema (required if dbusernamepwd is set")
	rootCmd.PersistentFlags().String("dbsuperuserpwd", "", "Password for the database superuser (required if dbusername is set")
	rootCmd.MarkFlagsRequiredTogether("dbsuperuser", "dbsuperuserpwd")
	viper.BindPFlag("dbconn.connstring", rootCmd.PersistentFlags().Lookup("dbconnstring"))
	viper.BindPFlag("dbconn.port", rootCmd.PersistentFlags().Lookup("dbport"))
	viper.BindPFlag("dbconn.database", rootCmd.PersistentFlags().Lookup("dbdb"))
	viper.BindPFlag("dbconn.schema", rootCmd.PersistentFlags().Lookup("dbschema"))
	viper.BindPFlag("dbconn.username", rootCmd.PersistentFlags().Lookup("dbusername"))
	viper.BindPFlag("dbconn.password", rootCmd.PersistentFlags().Lookup("dbpassword"))
	viper.BindPFlag("dbconn.superuser", rootCmd.PersistentFlags().Lookup("dbsuperuser"))
	viper.BindPFlag("dbconn.superuserpwd", rootCmd.PersistentFlags().Lookup("dbsuperuserpwd"))

	// Cobra also supports local flags, which will only run
	// when this action is called directly.
	// rootCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}

// initConfig reads in config file and ENV variables if set.
func initConfig() {
	if cfgFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(cfgFile)
	} else {
		// Find home directory.
		home, err := os.UserHomeDir()
		cobra.CheckErr(err)

		// Search config in home directory with name ".skima" (without extension).
		viper.AddConfigPath(home)
		viper.AddConfigPath(".")
		viper.SetConfigType("yaml")
		viper.SetConfigName(".skima")
	}

	viper.SetEnvPrefix("skm")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv() // read in environment variables that match

	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err == nil {
		fmt.Fprintln(os.Stderr, "Using config file:", viper.ConfigFileUsed())
	} else {
		fmt.Println(err)
	}
}
