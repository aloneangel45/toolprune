package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// BatchElement represents one parsed entry inside a JSON-RPC 2.0 batch array.
type BatchElement struct {
	Msg    *mcp.Message // Validated JSON-RPC message (nil if ErrMsg != nil)
	ErrMsg *mcp.Message // Per-element -32600 Invalid Request response when element is invalid
}

// ParsedPayload represents either a single JSON-RPC 2.0 message or a non-empty batch array.
type ParsedPayload struct {
	IsBatch bool
	Single  *mcp.Message
	Batch   []BatchElement
}

// ParseJSONRPCPayload parses a trimmed JSON-RPC 2.0 payload into either a single message
// or a JSON-RPC 2.0 batch array. Empty batch arrays ([]) return mcp.ErrInvalidRequest (-32600).
func ParseJSONRPCPayload(trimmed []byte) (*ParsedPayload, error) {
	trimmed = bytes.TrimSpace(trimmed)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%w: empty JSON-RPC payload", mcp.ErrParse)
	}

	switch trimmed[0] {
	case '{':
		var msg mcp.Message
		if unmarshalErr := json.Unmarshal(trimmed, &msg); unmarshalErr != nil {
			if errors.Is(unmarshalErr, mcp.ErrInvalidRequestID) {
				return nil, fmt.Errorf("%w: %v", mcp.ErrInvalidRequest, unmarshalErr)
			}
			var syntaxErr *json.SyntaxError
			if errors.As(unmarshalErr, &syntaxErr) {
				return nil, fmt.Errorf("%w: %v", mcp.ErrParse, unmarshalErr)
			}
			var typeErr *json.UnmarshalTypeError
			if errors.As(unmarshalErr, &typeErr) {
				return nil, fmt.Errorf("%w: %v", mcp.ErrInvalidRequest, unmarshalErr)
			}
			return nil, fmt.Errorf("%w: %v", mcp.ErrParse, unmarshalErr)
		}
		if envErr := msg.ValidateEnvelope(); envErr != nil {
			return nil, envErr
		}
		return &ParsedPayload{IsBatch: false, Single: &msg}, nil

	case '[':
		var rawItems []json.RawMessage
		if err := json.Unmarshal(trimmed, &rawItems); err != nil {
			return nil, fmt.Errorf("%w: %v", mcp.ErrParse, err)
		}
		if len(rawItems) == 0 {
			return nil, fmt.Errorf("%w: empty JSON-RPC batch array", mcp.ErrInvalidRequest)
		}

		elements := make([]BatchElement, 0, len(rawItems))
		for _, rawItem := range rawItems {
			itemTrimmed := bytes.TrimSpace(rawItem)
			if len(itemTrimmed) == 0 || itemTrimmed[0] != '{' {
				elements = append(elements, BatchElement{
					ErrMsg: mcp.NewErrorMessage(nil, mcp.CodeInvalidRequest, "Invalid Request"),
				})
				continue
			}

			var m mcp.Message
			if uErr := json.Unmarshal(itemTrimmed, &m); uErr != nil {
				elements = append(elements, BatchElement{
					ErrMsg: mcp.NewErrorMessage(nil, mcp.CodeInvalidRequest, "Invalid Request"),
				})
				continue
			}

			if envErr := m.ValidateEnvelope(); envErr != nil {
				var errID *mcp.RequestID
				if m.ID != nil && !m.ID.IsNull() {
					idCopy := *m.ID
					errID = &idCopy
				}
				elements = append(elements, BatchElement{
					ErrMsg: mcp.NewErrorMessage(errID, mcp.CodeInvalidRequest, envErr.Error()),
				})
				continue
			}

			msgCopy := m
			elements = append(elements, BatchElement{Msg: &msgCopy})
		}

		return &ParsedPayload{IsBatch: true, Batch: elements}, nil

	default:
		var probe any
		if synErr := json.Unmarshal(trimmed, &probe); synErr != nil {
			return nil, fmt.Errorf("%w: %v", mcp.ErrParse, synErr)
		}
		return nil, fmt.Errorf("%w: non-object JSON-RPC payload is not supported", mcp.ErrInvalidRequest)
	}
}

// batchElementSession captures the single correlated response for one request inside a batch
// while forwarding server notifications to the underlying ClientSession.
type batchElementSession struct {
	ctx        context.Context
	underlying ClientSession
	expectedID mcp.RequestID
	respOnce   sync.Once
	respCh     chan *mcp.Message
}

func newBatchElementSession(ctx context.Context, underlying ClientSession, expectedID mcp.RequestID) *batchElementSession {
	return &batchElementSession{
		ctx:        ctx,
		underlying: underlying,
		expectedID: expectedID,
		respCh:     make(chan *mcp.Message, 1),
	}
}

func (b *batchElementSession) ID() string {
	if b.underlying != nil {
		return b.underlying.ID()
	}
	return "batch-session"
}

func (b *batchElementSession) Context() context.Context {
	return b.ctx
}

func (b *batchElementSession) Send(msg *mcp.Message) error {
	if msg != nil && msg.IsResponse() && msg.ID != nil && msg.ID.Equal(b.expectedID) {
		b.respOnce.Do(func() {
			b.respCh <- msg
		})
		return nil
	}
	if b.underlying != nil {
		return b.underlying.Send(msg)
	}
	return nil
}

// DispatchBatch executes each element in a parsed JSON-RPC 2.0 batch and returns responses
// for all non-notification elements in deterministic slot order.
func DispatchBatch(ctx context.Context, underlying ClientSession, dispatcher MessageDispatcher, elements []BatchElement) []*mcp.Message {
	slots := make([]*mcp.Message, len(elements))
	var wg sync.WaitGroup

	for i, elem := range elements {
		if elem.ErrMsg != nil {
			slots[i] = elem.ErrMsg
			continue
		}
		if elem.Msg == nil {
			continue
		}
		if elem.Msg.IsNotification() || elem.Msg.IsResponse() {
			msgCopy := elem.Msg
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = dispatcher.HandleClientMessage(underlying, msgCopy)
			}()
			continue
		}

		idx := i
		reqMsg := elem.Msg
		capture := newBatchElementSession(ctx, underlying, *reqMsg.ID)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := dispatcher.HandleClientMessage(capture, reqMsg); err != nil {
				_ = capture.Send(mcp.NewErrorMessage(reqMsg.ID, mcp.CodeInternalError, err.Error()))
			}
			select {
			case resp := <-capture.respCh:
				slots[idx] = resp
			case <-ctx.Done():
				slots[idx] = mcp.NewErrorMessage(reqMsg.ID, mcp.CodeInternalError, "batch request cancelled")
			}
		}()
	}

	wg.Wait()

	responses := make([]*mcp.Message, 0, len(elements))
	for _, resp := range slots {
		if resp != nil {
			responses = append(responses, resp)
		}
	}
	return responses
}
