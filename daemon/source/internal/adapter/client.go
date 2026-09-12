package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/konstantinrink/agentflare/daemon/source/internal/protocol"
)

const socketDeadline = time.Second

type socketClient struct{ path string }

type response struct {
	OK     bool               `json:"ok"`
	Result json.RawMessage    `json:"result"`
	Error  *protocol.APIError `json:"error"`
}

func (c socketClient) send(ctx context.Context, request protocol.Request, result any) (*protocol.APIError, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode service request: %w", err)
	}
	if len(payload) > protocol.MaxRequestBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds %d", ErrRequestTooLarge, len(payload), protocol.MaxRequestBytes)
	}
	dialer := net.Dialer{Timeout: socketDeadline}
	connection, err := dialer.DialContext(ctx, "unix", c.path)
	if err != nil {
		return nil, fmt.Errorf("dial service: %w", err)
	}
	defer connection.Close()
	deadline := time.Now().Add(socketDeadline)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set service deadline: %w", err)
	}
	if _, err := connection.Write(append(payload, '\n')); err != nil {
		return nil, fmt.Errorf("write service request: %w", err)
	}
	// Limit the reader before ReadBytes so a peer that never sends a newline
	// cannot grow the response buffer without bound.
	limited := io.LimitReader(connection, protocol.MaxResponseBytes+1)
	line, err := bufio.NewReader(limited).ReadBytes('\n')
	if len(line) > protocol.MaxResponseBytes {
		return nil, ErrResponseTooLarge
	}
	if err != nil {
		return nil, fmt.Errorf("read service response: %w", err)
	}
	var envelope response
	if err := json.Unmarshal(line, &envelope); err != nil {
		return nil, fmt.Errorf("decode service response: %w", err)
	}
	if !envelope.OK {
		if envelope.Error == nil {
			return nil, errors.New("service returned an invalid error envelope")
		}
		return envelope.Error, nil
	}
	if result != nil && len(envelope.Result) != 0 {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			return nil, fmt.Errorf("decode service result: %w", err)
		}
	}
	return nil, nil
}
