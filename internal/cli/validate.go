package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/kenmoini/mcp-machine/internal/config"
)

// defaultConfigPath is where serve and validate look for configuration.
const defaultConfigPath = "/data/config.yaml"

// loadConfig loads and validates the config at path.
func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// runValidate implements "validate -c path": it prints "ok" for a valid
// config, or the validation error to stderr with exit code 1.
func runValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("c", defaultConfigPath, "path to the configuration file")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if _, err := loadConfig(*path); err != nil {
		fmt.Fprintf(stderr, "mcp-machine validate: %v\n", err)
		return exitRuntime
	}
	fmt.Fprintln(stdout, "ok")
	return exitOK
}
