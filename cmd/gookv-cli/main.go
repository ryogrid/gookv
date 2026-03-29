package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/ryogrid/gookv/pkg/client"
)

const version = "0.1.0"

func main() {
	pd := flag.String("pd", "127.0.0.1:2379", "PD server address(es), comma-separated")
	addr := flag.String("addr", "", "Connect directly to KV node gRPC endpoint (bypasses PD)")
	batch := flag.String("c", "", "Execute statement(s) and exit")
	hexMode := flag.Bool("hex", false, "Start in hex display mode")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("gookv-cli v%s (go%s, %s/%s)\n", version, runtime.Version()[2:], runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}

	// --addr and --pd are mutually exclusive (check if --pd was explicitly set)
	pdExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "pd" {
			pdExplicit = true
		}
	})
	if *addr != "" && pdExplicit {
		fmt.Fprintln(os.Stderr, "ERROR: --addr and --pd are mutually exclusive")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Set default stderr for executor warnings
	defaultStderr = os.Stderr

	fmtr := NewFormatter(os.Stdout)
	fmtr.SetErrOut(os.Stderr)

	if *hexMode {
		fmtr.SetDisplayMode(DisplayHex)
	}

	var exec *Executor

	if *addr != "" {
		// Direct connection mode: bypass PD, connect to KV node directly
		direct, err := newDirectRawKV(*addr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: connect to %s: %v\n", *addr, err)
			os.Exit(1)
		}
		defer func() { _ = direct.Close() }()

		exec = newTestExecutor(direct, nil, nil)
	} else {
		// PD connection mode (default)
		endpoints := splitEndpoints(*pd)

		c, err := client.NewClient(ctx, client.Config{
			PDAddrs: endpoints,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: connect to PD: %v\n", err)
			os.Exit(1)
		}
		defer func() { _ = c.Close() }()

		exec = NewExecutor(c)
	}

	if *batch != "" {
		os.Exit(runBatch(ctx, exec, fmtr, *batch))
	}

	if !isTerminal() {
		os.Exit(runPipe(ctx, exec, fmtr))
	}

	displayAddr := *addr
	if displayAddr == "" {
		displayAddr = strings.Join(splitEndpoints(*pd), ",")
	}
	os.Exit(runREPL(ctx, exec, fmtr, displayAddr))
}

func splitEndpoints(s string) []string {
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
