package responsebuffer

import (
	"bytes"
	"io"
	"sync"
)

// Buffer is single-reader owned. Growth charges both old and new arrays during
// the copy; Close drops the allocation and returns its reservation.
type Buffer struct {
	budget *Budget
	limit  int
	data   []byte
	lease  *Lease
}

func New(b *Budget, limit int) *Buffer {
	if b == nil {
		b = NewRequest()
	}
	return &Buffer{budget: b, limit: limit}
}
func (b *Buffer) Bytes() []byte { return b.data }
func (b *Buffer) Len() int      { return len(b.data) }
func (b *Buffer) Cap() int      { return cap(b.data) }
func (b *Buffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-len(b.data) {
		return 0, ErrLimit
	}
	need := len(b.data) + len(p)
	if need > cap(b.data) {
		capacity := min(b.limit, max(need, max(256, cap(b.data)*2)))
		lease, err := b.budget.Reserve(capacity)
		if err != nil {
			return 0, err
		}
		data := make([]byte, len(b.data), capacity)
		copy(data, b.data)
		b.lease.Release()
		b.data, b.lease = data, lease
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
func (b *Buffer) Close() error { b.data = nil; b.lease.Release(); b.lease = nil; return nil }

// Body takes ownership without copying. Borrowing keeps the reservation live
// even when cancellation closes the response while a decoder uses its bytes.
func (b *Buffer) Body() *Body {
	a := &allocation{data: b.data, lease: b.lease, refs: 1}
	body := &Body{allocation: a, reader: bytes.NewReader(b.data), budget: b.budget}
	b.data, b.lease = nil, nil
	return body
}

type allocation struct {
	mu    sync.Mutex
	data  []byte
	lease *Lease
	refs  int
}

func (a *allocation) release() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refs--
	if a.refs == 0 {
		a.data = nil
		a.lease.Release()
		a.lease = nil
	}
}

type Body struct {
	mu         sync.Mutex
	allocation *allocation
	reader     *bytes.Reader
	budget     *Budget
}

func (b *Body) ResponseBudget() *Budget { return b.budget }
func (b *Body) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reader == nil {
		return 0, io.ErrClosedPipe
	}
	return b.reader.Read(p)
}
func (b *Body) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.allocation != nil {
		b.allocation.release()
		b.allocation = nil
		b.reader = nil
	}
	return nil
}
func (b *Body) BorrowBytes() ([]byte, func(), bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.allocation == nil || b.reader.Len() != int(b.reader.Size()) {
		return nil, nil, false
	}
	a := b.allocation
	a.mu.Lock()
	a.refs++
	data := a.data
	a.mu.Unlock()
	var once sync.Once
	return data, func() { once.Do(a.release) }, true
}

// Borrow returns the complete buffered body only if reading has not begun.
func Borrow(source io.ReadCloser) ([]byte, func(), bool) {
	if b, ok := source.(interface{ BorrowBytes() ([]byte, func(), bool) }); ok {
		return b.BorrowBytes()
	}
	return nil, nil, false
}

// BudgetOf reads the memory account of a byte body or message-oriented source.
func BudgetOf(source any) *Budget {
	if b, ok := source.(interface{ ResponseBudget() *Budget }); ok {
		if budget := b.ResponseBudget(); budget != nil {
			return budget
		}
	}
	return NewRequest()
}

type budgetBody struct {
	io.ReadCloser
	budget *Budget
}

func (b *budgetBody) ResponseBudget() *Budget             { return b.budget }
func (b *budgetBody) BorrowBytes() ([]byte, func(), bool) { return Borrow(b.ReadCloser) }

// AttachBudget attaches a request account to an unread non-streaming body.
func AttachBudget(body io.ReadCloser, budget *Budget) io.ReadCloser {
	return &budgetBody{ReadCloser: body, budget: budget}
}

// ReadAll bounds decompressed input and returns partial bytes on errors. A
// one-byte sentinel distinguishes an exact-limit body from a larger response.
func ReadAll(source io.Reader, budget *Budget, limit int) (*Body, error) {
	buffer := New(budget, limit)
	scratchLease, err := buffer.budget.Reserve(32 << 10)
	if err != nil {
		return buffer.Body(), err
	}
	defer scratchLease.Release()
	scratch := make([]byte, 32<<10)
	emptyReads := 0
	for {
		n, readErr := source.Read(scratch[:min(len(scratch), limit-buffer.Len()+1)])
		if n > 0 {
			if _, err := buffer.Write(scratch[:n]); err != nil {
				return buffer.Body(), err
			}
			emptyReads = 0
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return buffer.Body(), io.ErrNoProgress
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				readErr = nil
			}
			return buffer.Body(), readErr
		}
	}
}
