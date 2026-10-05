package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/multica-ai/multica/server/internal/fleet/operator"
)

// The caller must launch this private command with a child-specific environment
// that omits HOME and PG settings. Validation denies ambient credential discovery.
// execute exposes only a bounded status, never private paths, URLs or errors.
func execute(ctx context.Context, args []string, out io.Writer, runner func(context.Context, string, operator.Config) error) int {
	deny := func() int { fmt.Fprintln(out, "fleet-env: denied"); return 1 }
	if len(args) < 1 {
		return deny()
	}
	action := args[0]
	fs := flag.NewFlagSet("fleet-env", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var cfg operator.Config
	fs.StringVar(&cfg.Namespace, "namespace", "", "")
	fs.StringVar(&cfg.FleetID, "fleet-id", "", "")
	fs.StringVar(&cfg.FleetURL, "fleet-url", "", "")
	fs.StringVar(&cfg.DatabaseURL, "database-url", "", "")
	fs.StringVar(&cfg.ConfigFile, "config", "", "")
	fs.StringVar(&cfg.ServiceKeyFile, "service-key", "", "")
	fs.StringVar(&cfg.ProfilesFile, "profiles", "", "")
	fs.StringVar(&cfg.OperationKey, "operation-key", "", "")
	fs.DurationVar(&cfg.Timeout, "timeout", 5*time.Minute, "")
	if fs.Parse(args[1:]) != nil || fs.NArg() != 0 || operator.Validate(action, cfg) != nil || runner == nil {
		return deny()
	}
	if err := runner(ctx, action, cfg); err != nil {
		return deny()
	}
	fmt.Fprintln(out, "fleet-env: "+action+" complete")
	return 0
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(execute(ctx, os.Args[1:], os.Stdout, operator.Run))
}
