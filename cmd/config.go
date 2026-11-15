package cmd

import (
	"github.com/johnmuhlestein/skima/internal/db"
	"github.com/johnmuhlestein/skima/internal/plan"
	"github.com/spf13/viper"
)

// The configuration comes from the flags, the SKM_ environment variables and the config file (see initConfig) - cmd
// is the only package that reads it; the others are given what they need

// workdir is the manifest's working directory
func workdir() string {
	return viper.GetString("workdir")
}

// dbConfig is how to reach the database and the schema skima manages in it
func dbConfig() db.Config {
	return db.Config{
		Host:     viper.GetString("dbconn.connstring"),
		Port:     viper.GetInt("dbconn.port"),
		Database: viper.GetString("dbconn.database"),
		Schema:   viper.GetString("dbconn.schema"),
		User:     viper.GetString("dbconn.username"),
		Password: viper.GetString("dbconn.password"),
	}
}

// openDB opens the configured database - close it when done
func openDB() (*db.DB, error) {
	return db.Open(dbConfig())
}

// planner plans the manifest in the working directory for the configured schema
func planner() plan.Planner {
	return plan.Planner{Workdir: workdir(), Schema: viper.GetString("dbconn.schema")}
}
