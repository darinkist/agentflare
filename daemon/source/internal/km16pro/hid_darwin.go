//go:build darwin

package km16pro

import (
	"context"

	"github.com/go-macos/iokit/hid"
)

type systemEnumerator struct{}

func (systemEnumerator) Devices() ([]Transport, error) {
	devices, err := hid.Devices(hid.Filter{
		VendorID:   VendorID,
		ProductIDs: []uint16{ProductID},
		UsagePage:  UsagePage,
		Usage:      Usage,
	})
	if err != nil {
		return nil, err
	}
	result := make([]Transport, 0, len(devices))
	for _, device := range devices {
		result = append(result, nativeTransport{device: device})
	}
	return result, nil
}

type nativeTransport struct{ device *hid.Device }

func (t nativeTransport) Info() hid.Info { return t.device.Info() }
func (t nativeTransport) Open() error    { return t.device.Open() }
func (t nativeTransport) SetReport(kind hid.ReportKind, id byte, data []byte) error {
	return t.device.SetReport(kind, id, data)
}
func (t nativeTransport) Stream(ctx context.Context, deliver func([]byte)) error {
	return hid.Stream(ctx, func(_ *hid.Device, data []byte) { deliver(data) }, t.device)
}
func (t nativeTransport) Close() error { return t.device.Close() }
