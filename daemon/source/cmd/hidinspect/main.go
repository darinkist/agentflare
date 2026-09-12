// Command hidinspect reads the VIA lighting values exposed by the KM16-Pro.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/go-macos/iokit/hid"
)

const (
	vendorID  = 0x28e9
	productID = 0x3145
	usagePage = 0xff60
	usage     = 0x61
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hidinspect:", err)
		os.Exit(1)
	}
}

func run() error {
	off := flag.Bool("off", false, "turn off the RGB matrix, underglow and all 16 key LEDs")
	flag.Parse()
	devices, err := hid.Devices(hid.Filter{VendorID: vendorID, ProductIDs: []uint16{productID}, UsagePage: usagePage, Usage: usage})
	if err != nil {
		return err
	}
	if len(devices) != 1 {
		for _, device := range devices {
			_ = device.Close()
		}
		return fmt.Errorf("expected one matching interface, found %d", len(devices))
	}
	device := devices[0]
	defer device.Close()
	if err := device.Open(); err != nil {
		return err
	}

	streamContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	responses := make(chan []byte, 8)
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- hid.Stream(streamContext, func(_ *hid.Device, data []byte) {
			if len(data) == 32 {
				select {
				case responses <- append([]byte(nil), data...):
				default:
				}
			}
		}, device)
	}()
	time.Sleep(10 * time.Millisecond)

	queries := [][3]byte{
		{0x08, 0x01, 0x01}, // backlight brightness
		{0x08, 0x02, 0x01}, // RGB light brightness
		{0x08, 0x02, 0x02}, // RGB light effect
		{0x08, 0x03, 0x01}, // RGB matrix brightness
		{0x08, 0x03, 0x02}, // RGB matrix effect
	}
	if *off {
		commands := [][32]byte{
			{0x07, 0x02, 0x01, 0}, // RGB light brightness: zero
			{0x07, 0x03, 0x01, 0}, // RGB matrix brightness: zero
			{0x07, 0x02, 0x02, 0}, // RGB light effect: all off
			{0x07, 0x03, 0x02, 0}, // RGB matrix effect: all off
		}
		for index := byte(0); index < 16; index++ {
			commands = append(commands, [32]byte{0x07, 0x00, 0x01, index})
		}
		for _, request := range commands {
			if err := send(device, responses, request); err != nil {
				fmt.Printf("set %02x/%02x/%02x: %v\n", request[1], request[2], request[0], err)
			} else {
				fmt.Printf("set %02x/%02x/%02x: acknowledged\n", request[1], request[2], request[0])
			}
		}
	}
	for _, query := range queries {
		request := [32]byte{query[0], query[1], query[2]}
		if err := send(device, responses, request); err != nil {
			fmt.Printf("%02x/%02x/%02x: %v\n", query[1], query[2], query[0], err)
			continue
		}
		response := lastResponse
		fmt.Printf("channel=%d value=%d response=% x\n", query[1], query[2], response[:4])
	}
	cancel()
	<-streamDone
	return nil
}

var lastResponse []byte

func send(device *hid.Device, responses <-chan []byte, request [32]byte) error {
	if err := device.SetReport(hid.Output, 0, request[:]); err != nil {
		return err
	}
	select {
	case response := <-responses:
		if len(response) != 32 || response[0] != request[0] || response[1] != request[1] || response[2] != request[2] {
			return fmt.Errorf("unexpected response % x", response[:min(len(response), 4)])
		}
		lastResponse = response
		return nil
	case <-time.After(500 * time.Millisecond):
		return context.DeadlineExceeded
	}
}

func min(first, second int) int {
	if first < second {
		return first
	}
	return second
}
