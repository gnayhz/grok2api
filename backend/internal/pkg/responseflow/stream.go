// Package responseflow assembles each physical SSE stream once. Admission,
// observers and client encoders borrow the same events, with bounded retention.
package responseflow

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
)

const MaxEventBytes = 8 << 20
const readBufferBytes = 32 << 10

// Event is borrowed during a callback. Raw preserves the physical bytes;
// Data joins SSE data fields with newlines and excludes the framing prefix.
// A one-line data field borrows Raw without allocating another payload.
type Event struct {
	Raw            []byte
	Data           []byte
	Kind           []byte
	HasData        bool
	first          bool
	rawBuffer      *responsebuffer.Buffer
	dataBuffer     *responsebuffer.Buffer
	metadataLease  *responsebuffer.Lease
	workspaceLease *responsebuffer.Lease
}

// Fields walks already assembled metadata. Values borrow Raw. Repeated data
// fields retain their wire order; unknown fields and comments remain available.
func (e *Event) Fields(handle func(name, value []byte)) {
	data := e.Raw
	first := e.first
	for len(data) > 0 {
		line, rest, _ := bytes.Cut(data, []byte{'\n'})
		data = rest
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if first {
			line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
			first = false
		}
		if len(line) == 0 {
			continue
		}
		name, value, found := bytes.Cut(line, []byte{':'})
		if !found {
			value = nil
		}
		if len(name) > 0 && len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		handle(name, value)
	}
}

func (e *Event) release() {
	if e.rawBuffer != nil {
		_ = e.rawBuffer.Close()
	}
	if e.dataBuffer != nil {
		_ = e.dataBuffer.Close()
	}
	e.Raw, e.Data, e.Kind = nil, nil, nil
	e.metadataLease.Release()
	e.workspaceLease.Release()
}

type Stream struct {
	source       io.ReadCloser
	budget       *responsebuffer.Budget
	readMu       sync.Mutex
	consumeMu    sync.Mutex
	reader       *bufio.Reader
	readLease    *responsebuffer.Lease
	first        bool
	pendingErr   error
	held         []*Event
	heldBytes    int
	active       *Event
	activeOffset int
	observers    []func(*Event)
	validators   []func(*Event) error
	onClose      []func()
	closed       atomic.Bool
	closeOnce    sync.Once
	closeErr     error
}

func New(source io.ReadCloser, budget *responsebuffer.Budget) *Stream {
	if budget == nil {
		budget = responsebuffer.NewRequest()
	}
	return &Stream{source: source, budget: budget, first: true}
}
func (s *Stream) CanonicalStream() *Stream               { return s }
func (s *Stream) ResponseBudget() *responsebuffer.Budget { return s.budget }
func (s *Stream) ReadOutcome() error {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	return s.pendingErr
}
func FromReader(source io.Reader) *Stream {
	if s, ok := source.(interface{ CanonicalStream() *Stream }); ok {
		return s.CanonicalStream()
	}
	return nil
}

// Observe and OnClose must be installed before reading begins. Observers run
// only on physical assembly, never when retained events are replayed.
func (s *Stream) Observe(fn func(*Event)) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if fn != nil && !s.closed.Load() {
		s.observers = append(s.observers, fn)
	}
}

// Validate installs a protocol check before delivery. Physical observers still
// see rejected events for usage and diagnostics; retained replay is not checked twice.
func (s *Stream) Validate(fn func(*Event) error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if fn != nil && !s.closed.Load() {
		s.validators = append(s.validators, fn)
	}
}
func (s *Stream) OnClose(fn func()) {
	s.readMu.Lock()
	closed := s.closed.Load()
	if !closed && fn != nil {
		s.onClose = append(s.onClose, fn)
	}
	s.readMu.Unlock()
	if closed && fn != nil {
		fn()
	}
}

// Hold retains physical events up to limit. A true result from inspect ends
// admission immediately, before another event is read or observed. On success
// Read/Consume replay those events without a second assembly or observer call.
func (s *Stream) Hold(limit int, inspect func(*Event) (bool, error)) error {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if s.active != nil {
		return errors.New("response stream already being delivered")
	}
	for {
		remaining := limit - s.heldBytes
		if remaining <= 0 {
			return responsebuffer.ErrLimit
		}
		event, err := s.nextPhysical(min(MaxEventBytes, remaining))
		if err != nil {
			return err
		}
		s.held = append(s.held, event)
		s.heldBytes += len(event.Raw)
		stop, err := inspect(event)
		if stop || err != nil {
			return err
		}
	}
}

func (s *Stream) next() (*Event, error) { return s.nextBounded(MaxEventBytes) }

func (s *Stream) nextBounded(limit int) (*Event, error) {
	if s.closed.Load() {
		return nil, io.ErrClosedPipe
	}
	if len(s.held) > 0 {
		event := s.held[0]
		s.held[0] = nil
		s.held = s.held[1:]
		s.heldBytes -= len(event.Raw)
		if len(event.Raw) > limit {
			event.release()
			return nil, responsebuffer.ErrLimit
		}
		return event, nil
	}
	return s.nextPhysical(limit)
}

func (s *Stream) nextPhysical(limit int) (*Event, error) {
	if s.closed.Load() {
		return nil, io.ErrClosedPipe
	}
	if s.pendingErr != nil {
		return nil, s.pendingErr
	}
	if s.reader == nil {
		lease, err := s.budget.Reserve(readBufferBytes)
		if err != nil {
			return nil, err
		}
		s.readLease = lease
		s.reader = bufio.NewReaderSize(s.source, readBufferBytes)
	}
	metadataLease, err := s.budget.Reserve(256)
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			metadataLease.Release()
		}
	}()
	raw := responsebuffer.New(s.budget, limit)
	lineStart := 0
	for {
		part, readErr := s.reader.ReadSlice('\n')
		if _, err := raw.Write(part); err != nil {
			_ = raw.Close()
			return nil, err
		}
		if readErr == bufio.ErrBufferFull {
			continue
		}
		line := raw.Bytes()[lineStart:]
		line = bytes.TrimSuffix(line, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if s.first && lineStart == 0 {
			line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
		}
		lineStart = raw.Len()
		if readErr != nil {
			s.pendingErr = readErr
		}
		if len(line) == 0 || readErr != nil {
			if raw.Len() == 0 {
				_ = raw.Close()
				return nil, readErr
			}
			event, err := assemble(raw, s.first, s.budget)
			s.first = false
			if err != nil {
				_ = raw.Close()
				return nil, err
			}
			event.metadataLease = metadataLease
			transferred = true
			s.observe(event)
			for _, validate := range s.validators {
				if err := validate(event); err != nil {
					event.release()
					s.pendingErr = err
					return nil, err
				}
			}
			return event, nil
		}
	}
}

func (s *Stream) observe(event *Event) {
	complete := false
	defer func() {
		if !complete {
			event.release()
		}
	}()
	for _, observe := range s.observers {
		observe(event)
	}
	complete = true
}

func assemble(raw *responsebuffer.Buffer, first bool, budget *responsebuffer.Budget) (*Event, error) {
	event := &Event{Raw: raw.Bytes(), rawBuffer: raw, first: first}
	var err error
	event.Fields(func(name, value []byte) {
		if err != nil {
			return
		}
		switch string(name) {
		case "event":
			event.Kind = value
		case "data":
			if !event.HasData {
				event.Data = value
				event.HasData = true
				return
			}
			if event.dataBuffer == nil {
				event.dataBuffer = responsebuffer.New(budget, MaxEventBytes)
				_, err = event.dataBuffer.Write(event.Data)
			}
			if err == nil {
				_, err = event.dataBuffer.Write([]byte{'\n'})
			}
			if err == nil {
				_, err = event.dataBuffer.Write(value)
			}
		}
	})
	if err != nil {
		event.release()
		return nil, err
	}
	if event.dataBuffer != nil {
		event.Data = event.dataBuffer.Bytes()
	}
	if event.HasData {
		lease, err := responsebuffer.JSONWorkspace(budget, event.Data)
		if err != nil {
			event.release()
			return nil, err
		}
		event.workspaceLease = lease
	}
	return event, nil
}

func (s *Stream) Read(p []byte) (int, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if s.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	if s.active == nil {
		event, err := s.next()
		if err != nil {
			return 0, err
		}
		s.active = event
	}
	n := copy(p, s.active.Raw[s.activeOffset:])
	s.activeOffset += n
	if s.activeOffset == len(s.active.Raw) {
		s.active.release()
		s.active = nil
		s.activeOffset = 0
	}
	return n, nil
}

func (s *Stream) Consume(handle func(*Event) error) error { return s.consume(0, handle) }

// ConsumeLimit caps total raw event bytes, including admission replay. The
// remaining budget also bounds the next event while it is being assembled.
func (s *Stream) ConsumeLimit(limit int, handle func(*Event) error) error {
	if limit <= 0 {
		return responsebuffer.ErrLimit
	}
	return s.consume(limit, handle)
}

func (s *Stream) consume(limit int, handle func(*Event) error) error {
	s.consumeMu.Lock()
	defer s.consumeMu.Unlock()
	remaining := limit
	for {
		eventLimit := MaxEventBytes
		if limit > 0 {
			eventLimit = min(eventLimit, remaining)
		}
		event, err := s.nextForConsumer(eventLimit)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if limit > 0 {
			remaining -= len(event.Raw)
		}
		// A blocked client writer must not hold the upstream close lock. The
		// event keeps its own reservation until this consumer releases it.
		err = consumeEvent(event, handle)
		if err != nil {
			return err
		}
	}
}

func (s *Stream) nextForConsumer(limit int) (*Event, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if s.active != nil {
		return nil, errors.New("cannot encode a partially delivered SSE event")
	}
	return s.nextBounded(limit)
}

func consumeEvent(event *Event, handle func(*Event) error) error {
	defer event.release()
	return handle(event)
}

// Consume borrows an existing physical assembler when available. Standalone
// converter callers receive the same framing and limits through a local owner.
func Consume(source io.Reader, handle func(*Event) error) error {
	if stream := FromReader(source); stream != nil {
		return stream.Consume(handle)
	}
	stream := New(io.NopCloser(source), nil)
	defer stream.Close()
	return stream.Consume(handle)
}

func (s *Stream) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.closeErr = s.source.Close() // interrupt blocking I/O before waiting for the reader
		s.readMu.Lock()
		for _, event := range s.held {
			event.release()
		}
		if s.active != nil {
			s.active.release()
		}
		s.held, s.active, s.reader = nil, nil, nil
		s.readLease.Release()
		callbacks := s.onClose
		s.onClose, s.observers, s.validators = nil, nil, nil
		s.readMu.Unlock()
		for _, fn := range callbacks {
			fn()
		}
	})
	return s.closeErr
}
