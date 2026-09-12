//go:build !darwin

package km16pro

import (
	"context"

	"github.com/go-macos/iokit/hid"
)

type systemEnumerator struct{}

func (systemEnumerator) Devices() ([]Transport, error) { return nil, hid.ErrUnsupported }

type nativeTransport struct{}

func (nativeTransport) Info() hid.Info { return hid.Info{} }
func (nativeTransport) Open() error    { return hid.ErrUnsupported }
func (nativeTransport) SetReport(hid.ReportKind, byte, []byte) error {
	return hid.ErrUnsupported
}
func (nativeTransport) Stream(context.Context, func([]byte)) error { return hid.ErrUnsupported }
func (nativeTransport) Close() error                               { return nil }
