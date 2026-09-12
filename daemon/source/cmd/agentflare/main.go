// Command agentflare runs the local AgentFlare daemon or talks to it through
// its Unix socket.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/konstantinrink/agentflare/daemon/source/internal/core"
	"github.com/konstantinrink/agentflare/daemon/source/internal/km16pro"
	"github.com/konstantinrink/agentflare/daemon/source/internal/protocol"
	"github.com/konstantinrink/agentflare/daemon/source/internal/server"
)

const clientTimeout = 5 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "agentflare:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agentflare daemon [--key-brightness N --underglow-brightness N] | emit <type> --source <source> --agent <agent> | lights set|clear | status [--v2] [--json] | clear")
	}
	socketPath, err := server.DefaultSocketPath()
	if err != nil {
		return err
	}
	switch args[0] {
	case "daemon":
		return runDaemonCommand(socketPath, args[1:])
	case "emit":
		return runEmit(socketPath, args[1:])
	case "status":
		return runStatus(socketPath, args[1:])
	case "clear":
		return runClear(socketPath, args[1:])
	case "lights":
		return runLights(socketPath, args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runDaemon(socketPath string) error {
	return runDaemonWithOptions(socketPath, core.DisplayOptions{
		KeyBrightness:       core.DefaultKeyBrightness,
		UnderglowBrightness: core.DefaultUnderglowBrightness,
	})
}

func runDaemonCommand(socketPath string, args []string) error {
	flags := flag.NewFlagSet("daemon", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	keyBrightness := flags.Int("key-brightness", core.DefaultKeyBrightness, "V2 subagent key brightness from 0 to 100")
	underglowBrightness := flags.Int("underglow-brightness", core.DefaultUnderglowBrightness, "V2 main-agent underglow brightness from 0 to 100")
	if countFlag(args, "key-brightness") > 1 || countFlag(args, "underglow-brightness") > 1 {
		return errors.New("duplicate flag")
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return errors.New("unexpected positional argument")
	}
	if *keyBrightness < 0 || *keyBrightness > 100 {
		return errors.New("--key-brightness must be from 0 to 100")
	}
	if *underglowBrightness < 0 || *underglowBrightness > 100 {
		return errors.New("--underglow-brightness must be from 0 to 100")
	}
	return runDaemonWithOptions(socketPath, core.DisplayOptions{
		KeyBrightness:       *keyBrightness,
		UnderglowBrightness: *underglowBrightness,
	})
}

func runDaemonWithOptions(socketPath string, displayOptions core.DisplayOptions) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	stateCore := core.NewWithDisplayOptions(core.NewRealClock(), displayOptions)
	coreDone := make(chan error, 1)
	go func() {
		coreDone <- stateCore.Run(ctx)
	}()

	socketServer := server.New(stateCore, server.Options{SocketPath: socketPath, Logger: logger})
	if err := socketServer.Listen(); err != nil {
		stop()
		<-coreDone
		return err
	}
	logger.Info("daemon started", "socket", socketPath)

	driver := km16pro.NewDefault(km16pro.Options{
		Logger: logger,
		StatusSink: func(statusContext context.Context, status km16pro.Status) error {
			return stateCore.SetDeviceStatus(statusContext, core.DeviceStatus(status))
		},
	})
	driverDone := make(chan error, 1)
	go func() { driverDone <- driver.Run(ctx, stateCore.Snapshots()) }()

	serverDone := make(chan error, 1)
	go func() { serverDone <- socketServer.Serve(ctx) }()

	var result, serverErr, driverErr error
	serverFinished := false
	driverFinished := false
	select {
	case err := <-serverDone:
		serverErr = err
		serverFinished = true
		if ctx.Err() == nil && err != nil {
			result = err
		}
	case err := <-driverDone:
		driverErr = err
		driverFinished = true
		if ctx.Err() == nil && err != nil {
			result = err
		}
	case <-ctx.Done():
	}
	stop()
	_ = socketServer.Close()
	if !serverFinished {
		serverErr = <-serverDone
	}
	if !driverFinished {
		driverErr = <-driverDone
	}
	coreErr := <-coreDone
	if result != nil {
		return result
	}
	if serverErr != nil {
		return serverErr
	}
	if driverErr != nil {
		return driverErr
	}
	if coreErr != nil {
		return coreErr
	}
	logger.Info("daemon stopped")
	return nil
}

func runEmit(socketPath string, args []string) error {
	if len(args) == 0 {
		return errors.New("event type is required")
	}
	flags := flag.NewFlagSet("emit", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	source := flags.String("source", "", "agent source")
	agentID := flags.String("agent", "", "agent ID")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return errors.New("unexpected positional argument")
	}
	request := protocol.Request{Version: protocol.Version, Type: protocol.EventType(args[0]), Source: *source, AgentID: *agentID}
	response, raw, err := sendRequest(socketPath, request)
	if err != nil {
		return err
	}
	return printEnvelope(response, raw)
}

func runStatus(socketPath string, args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "print JSON")
	v2Output := flags.Bool("v2", false, "show V2 display status")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return errors.New("unexpected positional argument")
	}
	version := protocol.Version
	if *v2Output {
		version = protocol.DisplayVersion
	}
	response, raw, err := sendRequest(socketPath, protocol.Request{Version: version, Type: protocol.Status})
	if err != nil {
		return err
	}
	if *jsonOutput {
		_, err = os.Stdout.Write(raw)
		return err
	}
	if !response.OK {
		return envelopeError(response)
	}
	if *v2Output {
		resultData, err := json.Marshal(response.Result)
		if err != nil {
			return err
		}
		var result protocol.DisplayStatusResult
		if err := json.Unmarshal(resultData, &result); err != nil {
			return err
		}
		printDisplayStatus(result)
		return nil
	}
	resultData, err := json.Marshal(response.Result)
	if err != nil {
		return err
	}
	var result protocol.StatusResult
	if err := json.Unmarshal(resultData, &result); err != nil {
		return err
	}
	fmt.Printf("device: %s\n", result.Device.Status)
	for _, slot := range result.Slots {
		identity := ""
		if slot.Source != "" || slot.AgentID != "" {
			identity = fmt.Sprintf(" %s/%s lease=%s", slot.Source, slot.AgentID, slot.LeaseEnd)
		}
		fmt.Printf("slot %d (LED %d): %s rgb=%d,%d,%d%s\n", slot.Slot, slot.LEDIndex, slot.State, slot.Color.R, slot.Color.G, slot.Color.B, identity)
	}
	for _, zone := range []string{"keys", "mmd", "backglow"} {
		fmt.Printf("%s:", zone)
		for _, light := range result.Lights {
			if light.Zone == zone {
				fmt.Printf(" %d=%d,%d,%d/%s", light.LEDIndex, light.Color.R, light.Color.G, light.Color.B, light.Layer)
			}
		}
		fmt.Println()
	}
	return nil
}

func printDisplayStatus(result protocol.DisplayStatusResult) {
	fmt.Printf("display: %s", result.Health)
	if result.Source != "" || result.SessionID != "" {
		fmt.Printf(" %s/%s", result.Source, result.SessionID)
	}
	fmt.Println()
	fmt.Printf("main: %s\n", result.MainState)
	for _, slot := range result.Slots {
		identity := ""
		if slot.Run != nil {
			identity = fmt.Sprintf(" %s/%s", slot.Run.AgentID, slot.Run.RunID)
		}
		fmt.Printf("slot %d (LED %d): %s%s\n", slot.Slot, slot.LEDIndex, slot.State, identity)
	}
	fmt.Printf("queue: %d\n", result.QueueLength)
	fmt.Printf("brightness: keys=%d underglow=%d\n", result.Brightness.Keys, result.Brightness.Underglow)
	fmt.Printf("device: %s\n", result.Device.Status)
}

func runClear(socketPath string, args []string) error {
	if len(args) != 0 {
		return errors.New("clear does not accept arguments")
	}
	response, _, err := sendRequest(socketPath, protocol.Request{Version: protocol.Version, Type: protocol.Clear})
	if err != nil {
		return err
	}
	return printEnvelope(response, nil)
}

func runLights(socketPath string, args []string) error {
	if len(args) == 0 {
		return errors.New("lights requires set or clear")
	}
	switch args[0] {
	case "set":
		return runLightsSet(socketPath, args[1:])
	case "clear":
		return runLightsClear(socketPath, args[1:])
	default:
		return fmt.Errorf("unknown lights command %q", args[0])
	}
}

func runLightsSet(socketPath string, args []string) error {
	flags := flag.NewFlagSet("lights set", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	zone := flags.String("zone", "", "LED zone")
	leds := flags.String("leds", "", "comma-separated LED indices")
	rgb := flags.String("rgb", "", "red,green,blue")
	ttl := flags.String("ttl", "", "Go duration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return errors.New("unexpected positional argument")
	}
	if countFlag(args, "zone") > 1 || countFlag(args, "leds") > 1 || countFlag(args, "rgb") > 1 || countFlag(args, "ttl") > 1 {
		return errors.New("duplicate flag")
	}
	target, err := parseCLITarget(flagProvided(args, "zone"), *zone, flagProvided(args, "leds"), *leds)
	if err != nil {
		return err
	}
	color, err := parseRGB(*rgb)
	if err != nil {
		return err
	}
	request := protocol.Request{Version: protocol.Version, Type: protocol.LightsSet, Target: &target, Color: &color}
	if flagProvided(args, "ttl") {
		duration, err := parseTTL(*ttl)
		if err != nil {
			return err
		}
		milliseconds := duration.Milliseconds()
		request.TTLMS = &milliseconds
	}
	response, _, err := sendRequest(socketPath, request)
	if err != nil {
		return err
	}
	return printEnvelope(response, nil)
}

func runLightsClear(socketPath string, args []string) error {
	flags := flag.NewFlagSet("lights clear", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	zone := flags.String("zone", "", "LED zone")
	leds := flags.String("leds", "", "comma-separated LED indices")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return errors.New("unexpected positional argument")
	}
	if countFlag(args, "zone") > 1 || countFlag(args, "leds") > 1 {
		return errors.New("duplicate flag")
	}
	target, err := parseCLITarget(flagProvided(args, "zone"), *zone, flagProvided(args, "leds"), *leds)
	if err != nil {
		return err
	}
	response, _, err := sendRequest(socketPath, protocol.Request{Version: protocol.Version, Type: protocol.LightsClear, Target: &target})
	if err != nil {
		return err
	}
	return printEnvelope(response, nil)
}

func countFlag(args []string, name string) int {
	count := 0
	for _, arg := range args {
		if arg == "--"+name || arg == "-"+name || strings.HasPrefix(arg, "--"+name+"=") || strings.HasPrefix(arg, "-"+name+"=") {
			count++
		}
	}
	return count
}

func flagProvided(args []string, name string) bool { return countFlag(args, name) != 0 }

func parseCLITarget(zoneProvided bool, zone string, ledsProvided bool, leds string) (protocol.Target, error) {
	if zoneProvided == ledsProvided {
		return protocol.Target{}, errors.New("exactly one of --zone or --leds is required")
	}
	if zoneProvided {
		if _, ok := core.LEDIndicesForZone(core.Zone(zone)); !ok {
			return protocol.Target{}, fmt.Errorf("invalid zone %q", zone)
		}
		return protocol.Target{Zone: zone}, nil
	}
	indices, err := parseLEDs(leds)
	if err != nil {
		return protocol.Target{}, err
	}
	return protocol.Target{LEDIndices: indices}, nil
}

func parseLEDs(value string) ([]int, error) {
	parts := strings.Split(value, ",")
	if len(parts) == 0 || len(parts) > core.LEDCount {
		return nil, errors.New("invalid LED list")
	}
	indices := make([]int, len(parts))
	seen := make(map[int]struct{}, len(parts))
	for i, part := range parts {
		if part == "" {
			return nil, errors.New("LED list contains an empty element")
		}
		index, err := strconv.Atoi(part)
		if err != nil || index < 0 || index >= core.LEDCount {
			return nil, fmt.Errorf("invalid LED index %q", part)
		}
		if _, exists := seen[index]; exists {
			return nil, fmt.Errorf("duplicate LED index %d", index)
		}
		seen[index] = struct{}{}
		indices[i] = index
	}
	sort.Ints(indices)
	return indices, nil
}

func parseRGB(value string) (protocol.Color, error) {
	if value == "" {
		return protocol.Color{}, errors.New("--rgb is required")
	}
	parts := strings.Split(value, ",")
	if len(parts) != 3 {
		return protocol.Color{}, errors.New("--rgb must contain three integers")
	}
	values := [3]uint8{}
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 || value > 255 {
			return protocol.Color{}, fmt.Errorf("invalid RGB channel %q", part)
		}
		values[i] = uint8(value)
	}
	return protocol.Color{R: values[0], G: values[1], B: values[2]}, nil
}

func parseTTL(value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration < time.Millisecond || duration > 24*time.Hour || duration%time.Millisecond != 0 {
		return 0, errors.New("--ttl must be a whole duration from 1ms to 24h")
	}
	return duration, nil
}

func sendRequest(socketPath string, request protocol.Request) (protocol.Envelope, []byte, error) {
	connection, err := net.DialTimeout("unix", socketPath, clientTimeout)
	if err != nil {
		return protocol.Envelope{}, nil, fmt.Errorf("connect daemon: %w", err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(clientTimeout))
	requestData, err := json.Marshal(request)
	if err != nil {
		return protocol.Envelope{}, nil, err
	}
	requestData = append(requestData, '\n')
	if _, err := connection.Write(requestData); err != nil {
		return protocol.Envelope{}, nil, fmt.Errorf("send request: %w", err)
	}
	line, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		return protocol.Envelope{}, nil, fmt.Errorf("read response: %w", err)
	}
	var response protocol.Envelope
	if err := json.Unmarshal(bytes.TrimSpace(line), &response); err != nil {
		return protocol.Envelope{}, nil, fmt.Errorf("decode response: %w", err)
	}
	return response, line, nil
}

func printEnvelope(response protocol.Envelope, _ []byte) error {
	if !response.OK {
		return envelopeError(response)
	}
	if response.Result == nil {
		return nil
	}
	data, err := json.Marshal(response.Result)
	if err != nil {
		return err
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err == nil {
		if indices, ok := result["led_indices"].([]any); ok {
			values := make([]string, len(indices))
			for i, index := range indices {
				values[i] = fmt.Sprint(index)
			}
			message := "leds " + strings.Join(values, ",")
			if expires, ok := result["expires_at"].(string); ok && expires != "" {
				message += " expires_at=" + expires
			}
			fmt.Println(message)
			return nil
		}
		if slot, ok := result["slot"]; ok {
			fmt.Printf("slot %v\n", slot)
			return nil
		}
		if cleared, ok := result["cleared"].(bool); ok && cleared {
			fmt.Println("cleared")
			return nil
		}
	}
	fmt.Println(strings.TrimSpace(string(data)))
	return nil
}

func envelopeError(response protocol.Envelope) error {
	if response.Error == nil {
		return errors.New("daemon returned an error")
	}
	return fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
}
