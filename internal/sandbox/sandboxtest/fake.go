// Package sandboxtest provides a scripted launcher for tests.
package sandboxtest

import (
	"context"
	"io"
	"sync"

	"github.com/iandeherdt/Maestro/internal/sandbox"
)

// Call is one recorded request.
type Call struct {
	Req   sandbox.Request
	Stdin []byte
}

// Fake records every request and answers through Handler.
type Fake struct {
	mu    sync.Mutex
	calls []Call
	// Handler answers a request. A nil Handler succeeds with no output.
	Handler func(ctx context.Context, req sandbox.Request) (sandbox.Result, error)
	// StreamHandler answers a streamed request. A nil StreamHandler
	// succeeds with no output.
	StreamHandler func(ctx context.Context, req sandbox.Request, stdout io.Writer) (sandbox.Result, error)
}

func (f *Fake) Stream(ctx context.Context, req sandbox.Request, stdin []byte, stdout io.Writer) (sandbox.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, Call{req, stdin})
	h := f.StreamHandler
	f.mu.Unlock()
	if h == nil {
		return sandbox.Result{}, nil
	}
	return h(ctx, req, stdout)
}

func (f *Fake) Run(ctx context.Context, req sandbox.Request, stdin []byte) (sandbox.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, Call{req, stdin})
	h := f.Handler
	f.mu.Unlock()
	if h == nil {
		return sandbox.Result{}, nil
	}
	return h(ctx, req)
}

func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Trace lists the calls as short strings such as "populate:clone" and
// "hook:after_create".
func (f *Fake) Trace() []string {
	var out []string
	for _, c := range f.Calls() {
		s := string(c.Req.Step)
		switch {
		case c.Req.Populate != nil:
			s += ":" + c.Req.Populate.Op
		case c.Req.Env["SYMPHONY_HOOK"] != "":
			s += ":" + c.Req.Env["SYMPHONY_HOOK"]
		}
		out = append(out, s)
	}
	return out
}
