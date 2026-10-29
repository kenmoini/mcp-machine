package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/kenmoini/mcp-machine/internal/config"
	"github.com/kenmoini/mcp-machine/internal/vault"
)

// Where serve and validate look for configuration by default.
const (
	defaultConfigPath  = "/etc/mcp-machine" // the admin root: config.yaml and config.d/
	defaultUserConfigD = "/data/config.d"   // the user drop-ins the web UI writes
)

// legacyConfigPath is the deprecated single-file location, read when the
// admin root holds no configuration. It is a variable only so tests can
// point it elsewhere.
var legacyConfigPath = "/data/config.yaml"

// configFlags are the configuration-location flags shared by serve and
// validate.
type configFlags struct{ path, systemDir, userDir string }

// addConfigFlags registers -c, --system-config-d and --config-d on fs.
func addConfigFlags(fs *flag.FlagSet) *configFlags {
	f := &configFlags{}
	fs.StringVar(&f.path, "c", defaultConfigPath,
		"admin configuration root (holding config.yaml and config.d/), or a single configuration file")
	fs.StringVar(&f.systemDir, "system-config-d", "",
		"admin drop-in directory (default <root>/config.d; none when -c names a file)")
	fs.StringVar(&f.userDir, "config-d", defaultUserConfigD,
		`user drop-in directory, written by the web UI ("" for none)`)
	return f
}

// layout resolves the flags into the configuration layout.
func (f *configFlags) layout() (config.Layout, error) {
	return config.ResolveLayout(config.LayoutOptions{
		Path:       f.path,
		SystemDir:  f.systemDir,
		UserDir:    f.userDir,
		LegacyFile: legacyConfigPath,
	})
}

// loadSet loads every layer of l and validates the merged configuration.
func loadSet(l config.Layout) (*config.Set, error) {
	set, err := l.Load()
	if err != nil {
		return nil, err
	}
	if err := config.Validate(set.Config); err != nil {
		return nil, err
	}
	return set, nil
}

// searched lists the paths a layout consulted, for messages.
func searched(set *config.Set) string { return strings.Join(set.Searched, ", ") }

// configWarning describes where set came from when that needs the operator's
// attention: the deprecated single-file location, or nowhere at all. It is
// "" otherwise.
func configWarning(set *config.Set) string {
	switch {
	case set == nil:
		return ""
	case set.Fallback != "" && len(set.Searched) > 0:
		// The fallback happens only in root mode, where the first path
		// searched is <root>/config.yaml.
		return fmt.Sprintf("configuration read from the deprecated location %s; move it to %s",
			set.Fallback, set.Searched[0])
	case len(set.Files) == 0:
		return fmt.Sprintf("no configuration found (searched: %s); running with defaults", searched(set))
	default:
		return ""
	}
}

// runValidate implements "validate": it prints "ok" for a valid
// configuration, or the error to stderr with exit code 1. Finding no
// configuration file at all is an error. With -v it also lists the files
// read and where each server is defined. With --vault it also reads every
// ${vault:...} reference from Vault and reports the ones that fail.
func runValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := addConfigFlags(fs)
	verbose := fs.Bool("v", false, "list the files read and where each server is defined")
	checkVault := fs.Bool("vault", false, "read every ${vault:...} reference from Vault and report the ones that fail")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "mcp-machine validate: %v\n", err)
		return exitRuntime
	}

	l, err := cf.layout()
	if err != nil {
		return fail(err)
	}
	set, err := loadSet(l)
	if err != nil {
		return fail(err)
	}
	if len(set.Files) == 0 {
		return fail(fmt.Errorf("no configuration found (searched: %s)", searched(set)))
	}
	if *checkVault {
		if problems := vaultProblems(context.Background(), set.Config); len(problems) > 0 {
			for _, p := range problems {
				fmt.Fprintf(stderr, "mcp-machine validate: %s\n", p)
			}
			return exitRuntime
		}
	}
	fmt.Fprintln(stdout, "ok")
	if *verbose {
		fmt.Fprintln(stdout, "files:")
		for _, f := range set.Files {
			fmt.Fprintf(stdout, "  %s\n", f)
		}
		fmt.Fprintln(stdout, "servers:")
		for _, s := range set.Config.Servers {
			fmt.Fprintf(stdout, "  %s: %s\n", s.Name, set.Origins[s.Name].String())
		}
	}
	return exitOK
}

// vaultProblems reads every ${vault:...} reference of cfg from Vault and
// returns a line per reference that cannot be read, or per failure before
// that (no Vault configured, authentication). A configuration without
// references never contacts Vault.
func vaultProblems(ctx context.Context, cfg *config.Config) []string {
	type ref struct {
		server string
		config.Ref
	}
	var refs []ref
	for _, r := range cfg.Registries {
		for _, v := range []string{r.Username, r.Password} {
			rs, _ := config.ParseRefs(v)
			for _, x := range rs {
				refs = append(refs, ref{"", x})
			}
		}
	}
	for _, s := range cfg.Servers {
		rs, _ := config.CheckVaultRefs(s)
		for _, x := range rs {
			refs = append(refs, ref{s.Name, x})
		}
	}
	if len(refs) == 0 {
		return nil
	}
	if !cfg.Vault.Configured() {
		return []string{"vault is not configured (no vault.address)"}
	}
	vc, err := vault.New(vault.FromConfig(cfg.Vault), slog.New(slog.DiscardHandler))
	if err != nil {
		return []string{err.Error()}
	}
	if err := vc.Start(ctx); err != nil {
		return []string{err.Error()}
	}
	defer vc.Close()
	var out []string
	rv := &vault.Resolver{Reader: vc}
	for _, r := range refs {
		if _, err := rv.Resolve(ctx, r.String()); err != nil {
			where := "registries"
			if r.server != "" {
				where = fmt.Sprintf("server %q", r.server)
			}
			out = append(out, fmt.Sprintf("%s: %v", where, err))
		}
	}
	return out
}
