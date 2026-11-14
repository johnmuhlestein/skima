package plan

import (
	"sync"

	"github.com/spf13/viper"
)

var workdirMu sync.Mutex

// InWorkdir runs fn with the configured working directory set to dir, then restores it. Plans - and the manifest
// definitions their deltas read - come from the configured working directory, so this is how to plan a manifest
// elsewhere, e.g. the baseline's state files exported from git. Calls are serialized: in a server, every plan made while
// requests run concurrently goes through InWorkdir, so each sees its own working directory. Don't nest calls
func InWorkdir(dir string, fn func() error) error {
	workdirMu.Lock()
	defer workdirMu.Unlock()
	previous := viper.GetString("workdir")
	viper.Set("workdir", dir)
	defer viper.Set("workdir", previous)
	return fn()
}
