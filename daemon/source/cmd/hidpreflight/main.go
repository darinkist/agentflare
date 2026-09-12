// Command hidpreflight validates the KM16-Pro HID interface and VIA protocol
// before the service is built on top of it.
package main

import (
	"context"
	"errors"
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
		fmt.Fprintln(os.Stderr, "hidpreflight:", err)
		os.Exit(1)
	}
}

func run() error {
	devices, err := hid.Devices(hid.Filter{
		VendorID:   vendorID,
		ProductIDs: []uint16{productID},
		UsagePage:  usagePage,
		Usage:      usage,
	})
	if err != nil {
		return fmt.Errorf("enumerate KM16-Pro interface: %w", err)
	}
	if len(devices) != 1 {
		for _, device := range devices {
			_ = device.Close()
		}
		return fmt.Errorf("expected exactly one matching KM16-Pro interface, found %d", len(devices))
	}
	device := devices[0]
	defer device.Close()

	if err := device.Open(); err != nil {
		return fmt.Errorf("open KM16-Pro interface: %w", err)
	}

	request := [32]byte{0x08, 0x03, 0x02}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	reports := make(chan []byte, 1)
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- hid.Stream(ctx, func(_ *hid.Device, data []byte) {
			if len(data) != len(request) {
				return
			}
			response := append([]byte(nil), data...)
			select {
			case reports <- response:
			case <-ctx.Done():
			}
		}, device)
	}()

	// Give the stream goroutine time to schedule the device on its run loop.
	// The protocol timeout remains the only correctness boundary.
	time.Sleep(10 * time.Millisecond)
	if err := device.SetReport(hid.Output, 0, request[:]); err != nil {
		cancel()
		<-streamDone
		return fmt.Errorf("send VIA RGB-matrix query: %w", err)
	}

	select {
	case response := <-reports:
		cancel()
		streamErr := <-streamDone
		if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
			return fmt.Errorf("read VIA response: %w", streamErr)
		}
		if response[0] != request[0] || response[1] != request[1] || response[2] != request[2] {
			return fmt.Errorf("unexpected VIA response header: % x", response[:3])
		}
		fmt.Printf("PASS: %s\nVIA response: % x\n", device.Info(), response)
		return nil
	case <-ctx.Done():
		<-streamDone
		return fmt.Errorf("wait for VIA response: %w", ctx.Err())
	}
}
