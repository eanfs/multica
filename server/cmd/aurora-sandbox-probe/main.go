// Command aurora-sandbox-probe is the fixed probe binary baked into the Aurora
// sandbox fixture image. The Linux Docker security acceptance runs its
// subcommands inside a provisioned sandbox through docker exec; each subcommand
// attempts exactly one operation and prints a single JSON result on stdout. A
// process the sandbox kills before it can print (for example a seccomp
// KILL_PROCESS rule) is itself a denial, which the acceptance test treats as
// blocked.
package main

import "os"

func main() {
	os.Exit(run(os.Args[1:]))
}
