// Command agentflare-adapter observes one selected harness session and sends
// normalized V2 display states to the local AgentFlare service.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/konstantinrink/agentflare/daemon/source/internal/adapter"
	"github.com/konstantinrink/agentflare/daemon/source/internal/adapter/replay"
	"github.com/konstantinrink/agentflare/daemon/source/internal/server"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "agentflare-adapter:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("agentflare-adapter", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	sourceName := flags.String("source", "", "event source, currently replay")
	fixturePath := flags.String("fixture", "", "replay fixture path")
	sessionID := flags.String("session", "", "explicit existing source session ID")
	socketPath := flags.String("socket", "", "AgentFlare service socket path")
	diagnostics := flags.Bool("diagnostics", false, "print privacy-safe diagnostics on exit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return errors.New("unexpected positional argument")
	}
	if *sourceName != "replay" {
		return errors.New("--source replay is currently the only available source")
	}
	if *fixturePath == "" || *sessionID == "" {
		return errors.New("--fixture and --session are required")
	}
	if *socketPath == "" {
		path, err := server.DefaultSocketPath()
		if err != nil {
			return err
		}
		*socketPath = path
	}
	fixture, err := replay.Load(*fixturePath)
	if err != nil {
		return err
	}
	source, err := replay.NewSource(fixture)
	if err != nil {
		return err
	}
	runtime, err := adapter.New(adapter.Options{Source: source, SessionID: *sessionID, SocketPath: *socketPath})
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err = runtime.Run(ctx)
	if *diagnostics {
		data, marshalErr := json.Marshal(runtime.Diagnostics())
		if marshalErr != nil {
			return marshalErr
		}
		_, _ = fmt.Fprintln(os.Stdout, string(data))
	}
	return err
}
