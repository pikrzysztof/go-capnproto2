// Package rpc implements the Cap'n Proto RPC protocol.
package rpc // import "capnproto.org/go/capnp/v3/rpc"

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"capnproto.org/go/capnp/v3"
	"capnproto.org/go/capnp/v3/exc"
	"capnproto.org/go/capnp/v3/exp/spsc"
	"capnproto.org/go/capnp/v3/internal/syncutil"
	"capnproto.org/go/capnp/v3/rpc/transport"
	rpccp "capnproto.org/go/capnp/v3/std/capnp/rpc"
)

/*
At a high level, Conn manages three resources:

1) The connection's state: the tables
2) The transport's outbound stream
3) The transport's inbound stream

Each of these resources require mutually exclusive access.  Complexity
ensues because there are two primary actors contending for these
resources: the local vat (sometimes referred to as the application) and
the remote vat.  In this implementation, the remote vat is represented
by a goroutine that is solely responsible for the inbound stream.  This
is referred to as the receive goroutine.  The local vat accesses the
Conn via objects created by the Conn, and may do so from many different
goroutines.  However, the Conn will largely serialize operations coming
from the local vat.  Similarly, outbound messages are enqueued on 'sendq',
and processed by a single goroutine.

Conn protects the connection state with a simple mutex: Conn.mu.  This
mutex must not be held while performing operations that take
indeterminate time or are provided by the application.  This reduces
contention, but more importantly, prevents deadlocks.  An application-
provided operation can (and commonly will) call back into the Conn.

The receive goroutine, being the only goroutine that receives messages
from the transport, can receive from the transport without additional
synchronization.  One intentional side effect of this arrangement is
that during processing of a message, no other messages will be received.
This provides backpressure to the remote vat as well as simplifying some
message processing.  However, it is essential that the receive goroutine
never block while processing a message.  In other words, the receive
goroutine may only block when waiting for an incoming message.

Some advice for those working on this code:

Many functions are verbose; resist the urge to shorten them.  There's
a lot more going on in this code than in most code, and many steps
require complicated invariants.  Only extract common functionality if
the preconditions are simple.

As much as possible, ensure that when a function returns, the goroutine
is holding (or not holding) the same set of locks as when it started.
Try to push lock acquisition as high up in the call stack as you can.
This makes it easy to see and avoid extraneous lock transitions, which
is a common source of errors and/or inefficiencies.
*/

// A Conn is a connection to another Cap'n Proto vat.
// It is safe to use from multiple goroutines.
type Conn struct {
	bootstrap    capnp.Client
	er           errReporter
	abortTimeout time.Duration

	// bgctx is a Context that is canceled when shutdown starts. Note
	// that it's parent is context.Background(), so we can rely on this
	// being the *only* time it will be canceled.
	bgctx context.Context

	// tasks block shutdown.
	tasks  sync.WaitGroup
	closed chan struct{} // closed when shutdown() returns

	// The underlying transport. Only the reader & writer goroutines
	// may do IO, but any thread may call NewMessage().
	transport Transport

	// Receive end of the send queue (lk.sendTx). Only the send goroutine may
	// touch this.
	sendRx *spsc.Rx[asyncSend]

	// lk contains all the fields that need to be protected by a mutex
	// this makes it easy to tell at call sites whether you should or
	// should not be holding the lock.
	lk struct {
		sync.Mutex // protects all the fields in lk.

		// The send end of the send queue; outgoing messages are inserted
		// here to be sent by a dedicated goroutine.
		sendTx *spsc.Tx[asyncSend]

		closing  bool               // used to make shutdown() idempotent
		bgcancel context.CancelFunc // bgcancel cancels bgctx.

		// Tables
		questions  []*question
		questionID idgen
		answers    map[answerID]*answer
		exports    []*expent
		exportID   idgen
		imports    map[importID]*impent
		embargoes  []*embargo
		embargoID  idgen
	}
}

// Options specifies optional parameters for creating a Conn.
type Options struct {
	// BootstrapClient is the capability that will be returned to the
	// remote peer when receiving a Bootstrap message.  NewConn "steals"
	// this reference: it will release the client when the connection is
	// closed.
	BootstrapClient capnp.Client

	// ErrorReporter will be called upon when errors occur while the Conn
	// is receiving messages from the remote vat.
	ErrorReporter ErrorReporter

	// AbortTimeout specifies how long to block on sending an abort message
	// before closing the transport.  If zero, then a reasonably short
	// timeout is used.
	AbortTimeout time.Duration
}

// ErrorReporter can receive errors from a Conn.  ReportError should be quick
// to return and should not use the Conn that it is attached to.
type ErrorReporter interface {
	ReportError(error)
}

// NewConn creates a new connection that communicates on a given transport.
//
// Closing the connection will close the transport and release the bootstrap
// client provided in opts.
//
// If opts == nil, sensible defaults are used. See Options for more info.
//
// Once a connection is created, it will immediately start receiving
// requests from the transport.
func NewConn(t Transport, opts *Options) *Conn {
	ctx, cancel := context.WithCancel(context.Background())

	// We use an errgroup to link the lifetime of background tasks
	// to each other.
	g, ctx := errgroup.WithContext(ctx)

	c := &Conn{
		transport: t,
		closed:    make(chan struct{}),
		bgctx:     ctx,
	}
	sender := spsc.New[asyncSend]()
	c.sendRx = &sender.Rx
	c.lk.sendTx = &sender.Tx

	c.lk.bgcancel = cancel
	c.lk.answers = make(map[answerID]*answer)
	c.lk.imports = make(map[importID]*impent)

	if opts != nil {
		c.bootstrap = opts.BootstrapClient
		c.er = errReporter{opts.ErrorReporter}
		c.abortTimeout = opts.AbortTimeout
	}
	if c.abortTimeout == 0 {
		c.abortTimeout = 100 * time.Millisecond
	}

	// start background tasks
	g.Go(c.backgroundTask(c.send))
	g.Go(c.backgroundTask(c.receive))

	// monitor background tasks
	go func() {
		err := g.Wait()

		// Treat context.Canceled as a success indicator.
		// Do not report or send an abort message.
		if errors.Is(err, context.Canceled) {
			err = nil
		}

		c.er.ReportError(err) // ignores nil errors

		if err = c.shutdown(err); err != nil {
			c.er.ReportError(err)
		}
	}()

	return c
}

func (c *Conn) backgroundTask(f func() error) func() error {
	c.tasks.Add(1)

	return func() (err error) {
		defer c.tasks.Done()

		// backgroundTask MUST return a non-nil error in order to signal
		// other tasks to stop.  The context.Canceled will be treated as
		// a success indicator by the caller.
		if err = f(); err == nil {
			err = context.Canceled
		}

		return err
	}
}

// Bootstrap returns the remote vat's bootstrap interface.  This creates
// a new client that the caller is responsible for releasing.
func (c *Conn) Bootstrap(ctx context.Context) (bc capnp.Client) {
	c.lk.Lock()
	defer c.lk.Unlock()

	// Start a background task to prevent the conn from shutting down
	// while sending the bootstrap message.
	if !c.startTask() {
		return capnp.ErrorClient(rpcerr.Disconnectedf("connection closed"))
	}
	defer c.tasks.Done()

	bootCtx, cancel := context.WithCancel(ctx)
	q := c.newQuestion(capnp.Method{})
	bc, q.bootstrapPromise = capnp.NewPromisedClient(bootstrapClient{
		c:      q.p.Answer().Client().AddRef(),
		cancel: cancel,
	})

	c.sendMessage(ctx, func(m rpccp.Message) error {
		boot, err := m.NewBootstrap()
		if err == nil {
			boot.SetQuestionId(uint32(q.id))
		}
		return err

	}, func(err error) {
		if err != nil {
			syncutil.With(&c.lk, func() {
				c.lk.questions[q.id] = nil
			})
			q.bootstrapPromise.Reject(exc.Annotate("rpc", "bootstrap", err))
			syncutil.With(&c.lk, func() {
				c.lk.questionID.remove(uint32(q.id))
			})
			return
		}

		c.tasks.Add(1)
		go func() {
			defer c.tasks.Done()
			q.handleCancel(bootCtx)
		}()
	})

	return
}

type bootstrapClient struct {
	c      capnp.Client
	cancel context.CancelFunc
}

func (bc bootstrapClient) Send(ctx context.Context, s capnp.Send) (*capnp.Answer, capnp.ReleaseFunc) {
	return bc.c.SendCall(ctx, s)
}

func (bc bootstrapClient) Recv(ctx context.Context, r capnp.Recv) capnp.PipelineCaller {
	return bc.c.RecvCall(ctx, r)
}

func (bc bootstrapClient) Brand() capnp.Brand {
	return bc.c.State().Brand
}

func (bc bootstrapClient) Shutdown() {
	bc.cancel()
	bc.c.Release()
}

// Close sends an abort to the remote vat and closes the underlying
// transport.
func (c *Conn) Close() error {
	return c.shutdown(exc.Exception{ // NOTE:  omit "rpc" prefix
		Type:  exc.Failed,
		Cause: ErrConnClosed,
	})
}

// Done returns a channel that is closed after the connection is
// shut down.
func (c *Conn) Done() <-chan struct{} {
	return c.closed
}

// shutdown tears down the connection and transport, optionally sending
// an abort message before closing.  The caller MUST NOT hold c.lk.
// shutdown is idempotent.
func (c *Conn) shutdown(abortErr error) (err error) {
	alreadyClosing := false

	syncutil.With(&c.lk, func() {
		alreadyClosing = c.lk.closing
		if !alreadyClosing {
			c.lk.closing = true
			c.lk.bgcancel()
			c.cancelTasks()
		}
	})

	if !alreadyClosing {
		readyForClose := make(chan struct{})
		go func() {
			defer close(c.closed)
			select {
			case <-readyForClose:
			case <-time.After(c.abortTimeout):
			}
			if err = c.transport.Close(); err != nil {
				err = rpcerr.Failedf("close transport: %w", err)
			}
		}()

		c.tasks.Wait()
		c.drainQueue()

		rl := &releaseList{}
		syncutil.With(&c.lk, func() {
			c.release(rl)
		})
		rl.Release()
		c.abort(abortErr)
		close(readyForClose)
	}
	<-c.closed

	return
}

// Cancel all tasks and prevent new tasks from being started.
// Does not wait for tasks to finish shutting down.
// Called by 'shutdown'.  Callers MUST hold c.lk.
func (c *Conn) cancelTasks() {
	for _, a := range c.lk.answers {
		if a != nil && a.cancel != nil {
			a.cancel()
		}
	}
}

// caller MUST NOT hold c.lk
func (c *Conn) drainQueue() {
	for {
		pending, ok := c.sendRx.TryRecv()
		if !ok {
			break
		}

		pending.Abort(ErrConnClosed)
	}
}

// Clear all tables, and arrange for the releaseList to release exported clients
// and unfinished answers. Called by 'shutdown'.  Caller MUST hold c.lk.
func (c *Conn) release(rl *releaseList) {
	exports := c.lk.exports
	embargoes := c.lk.embargoes
	answers := c.lk.answers
	questions := c.lk.questions
	c.lk.imports = nil
	c.lk.exports = nil
	c.lk.embargoes = nil
	c.lk.questions = nil
	c.lk.answers = nil

	c.releaseBootstrap(rl)
	c.releaseExports(rl, exports)
	c.liftEmbargoes(rl, embargoes)
	c.releaseAnswers(rl, answers)
	c.releaseQuestions(rl, questions)

}

func (c *Conn) releaseBootstrap(rl *releaseList) {
	rl.Add(c.bootstrap.Release)
	c.bootstrap = capnp.Client{}
}

func (c *Conn) releaseExports(rl *releaseList, exports []*expent) {
	for _, e := range exports {
		if e != nil {
			metadata := e.client.State().Metadata
			syncutil.With(metadata, func() {
				c.clearExportID(metadata)
			})

			rl.Add(e.client.Release)
		}
	}
}

func (c *Conn) liftEmbargoes(rl *releaseList, embargoes []*embargo) {
	for _, e := range embargoes {
		if e != nil {
			rl.Add(e.lift)
		}
	}
}

func (c *Conn) releaseAnswers(rl *releaseList, answers map[answerID]*answer) {
	for _, a := range answers {
		if a != nil && a.msgReleaser != nil {
			rl.Add(a.msgReleaser.Decr)
		}
	}
}

func (c *Conn) releaseQuestions(rl *releaseList, questions []*question) {
	for _, q := range questions {
		canceled := q != nil && q.flags&finished != 0
		if !canceled {
			// Only reject the question if it isn't already flagged
			// as finished; otherwise it was rejected when the finished
			// flag was set.

			qr := q // Capture a different variable each time through the loop.
			rl.Add(func() {
				qr.Reject(ExcClosed)
			})
		}
	}
}

// If abortErr != nil, send abort message.  IO and alloc errors are ignored.
// Called by 'shutdown'.  Callers MUST NOT hold c.lk.
func (c *Conn) abort(abortErr error) {
	// send abort message?
	if abortErr != nil {
		outMsg, err := c.transport.NewMessage()
		if err != nil {
			return
		}
		defer outMsg.Release()

		// configure & send abort message
		if abort, err := outMsg.Message.NewAbort(); err == nil {
			abort.SetType(rpccp.Exception_Type(exc.TypeOf(abortErr)))
			if err = abort.SetReason(abortErr.Error()); err == nil {
				outMsg.Send()
			}
		}
	}
}

func (c *Conn) send() error {
	for {
		async, err := c.sendRx.Recv(c.bgctx)
		if err != nil {
			return err
		}

		async.Send()
	}
}

// receive receives and dispatches messages coming from c.transport.  receive
// runs in a background goroutine.
//
// After receive returns, the connection is shut down.  If receive
// returns a non-nil error, it is sent to the remove vat as an abort.
func (c *Conn) receive() error {
	ctx := c.bgctx

	incoming := make(chan incomingMessage)
	// We delegate actual IO to a separate goroutine, so we can always be responsive to the
	// context:
	go reader(ctx, incoming, c.transport)

	for {
		var (
			recv    rpccp.Message
			release capnp.ReleaseFunc
			err     error
		)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case in := <-incoming:
			recv = in.Message
			release = in.Release
			err = in.err
		}

		if err != nil {
			return err
		}

		switch recv.Which() {
		case rpccp.Message_Which_unimplemented:
			// no-op for now to avoid feedback loop

		case rpccp.Message_Which_abort:
			defer release()

			e, err := recv.Abort()
			if err != nil {
				c.er.ReportError(fmt.Errorf("read abort: %w", err))
				return nil
			}

			reason, err := e.Reason()
			if err != nil {
				c.er.ReportError(fmt.Errorf("read abort: reason: %w", err))
				return nil
			}

			c.er.ReportError(exc.New(exc.Type(e.Type()), "rpc", "remote abort: "+reason))
			return nil

		case rpccp.Message_Which_bootstrap:
			bootstrap, err := recv.Bootstrap()
			if err != nil {
				release()
				c.er.ReportError(fmt.Errorf("read bootstrap: %w", err))
				continue
			}
			qid := answerID(bootstrap.QuestionId())
			release()
			if err := c.handleBootstrap(ctx, qid); err != nil {
				return err
			}

		case rpccp.Message_Which_call:
			call, err := recv.Call()
			if err != nil {
				release()
				c.er.ReportError(fmt.Errorf("read call: %w", err))
				continue
			}
			if err := c.handleCall(ctx, call, release); err != nil {
				return err
			}

		case rpccp.Message_Which_return:
			ret, err := recv.Return()
			if err != nil {
				release()
				c.er.ReportError(fmt.Errorf("read return: %w", err))
				continue
			}
			if err := c.handleReturn(ctx, ret, release); err != nil {
				return err
			}

		case rpccp.Message_Which_finish:
			fin, err := recv.Finish()
			if err != nil {
				release()
				c.er.ReportError(fmt.Errorf("read finish: %w", err))
				continue
			}
			qid := answerID(fin.QuestionId())
			releaseResultCaps := fin.ReleaseResultCaps()
			release()
			if err := c.handleFinish(ctx, qid, releaseResultCaps); err != nil {
				return err
			}

		case rpccp.Message_Which_release:
			rel, err := recv.Release()
			if err != nil {
				release()
				c.er.ReportError(fmt.Errorf("read release: %w", err))
				continue
			}
			id := exportID(rel.Id())
			count := rel.ReferenceCount()
			release()
			if err := c.handleRelease(ctx, id, count); err != nil {
				return err
			}

		case rpccp.Message_Which_disembargo:
			d, err := recv.Disembargo()
			if err != nil {
				release()
				c.er.ReportError(fmt.Errorf("read disembargo: %w", err))
				continue
			}
			err = c.handleDisembargo(ctx, d, release)
			if err != nil {
				return err
			}

		default:
			c.er.ReportError(fmt.Errorf("unknown message type %v from remote", recv.Which()))
			c.sendMessage(ctx, func(m rpccp.Message) error {
				defer release()
				if err := m.SetUnimplemented(recv); err != nil {
					return rpcerr.Annotatef(err, "send unimplemented")
				}
				return nil
			}, nil)
		}
	}
}

func (c *Conn) handleBootstrap(ctx context.Context, id answerID) error {
	rl := &releaseList{}
	defer rl.Release()

	var (
		err error
		ans = answer{c: c, id: id}
	)

	ans.ret, ans.sendMsg, ans.msgReleaser, err = c.newReturn()
	if err == nil {
		ans.ret.SetAnswerId(uint32(id))
		ans.ret.SetReleaseParamCaps(false)
	}

	syncutil.With(&c.lk, func() {
		if c.lk.answers[id] != nil {
			rl.Add(ans.msgReleaser.Decr)
			err = rpcerr.Failedf("incoming bootstrap: answer ID %d reused", id)
			return
		}

		if err != nil {
			err = rpcerr.Annotate(err, "incoming bootstrap")
			c.lk.answers[id] = errorAnswer(c, id, err)
			c.er.ReportError(err)
			return
		}

		c.lk.answers[id] = &ans
		if !c.bootstrap.IsValid() {
			ans.sendException(rl, exc.New(exc.Failed, "", "vat does not expose a public/bootstrap interface"))
			return
		}
		if err := ans.setBootstrap(c.bootstrap.AddRef()); err != nil {
			ans.sendException(rl, err)
			return
		}
		err = ans.sendReturn(rl)
		if err != nil {
			// Answer cannot possibly encounter a Finish, since we still
			// haven't returned to receive().
			panic(err)
		}
	})
	return err
}

func (c *Conn) handleCall(ctx context.Context, call rpccp.Call, releaseCall capnp.ReleaseFunc) error {
	rl := &releaseList{}
	defer rl.Release()

	id := answerID(call.QuestionId())

	// TODO(3rd-party handshake): support sending results to 3rd party vat
	if call.SendResultsTo().Which() != rpccp.Call_sendResultsTo_Which_caller {
		// TODO(someday): handle SendResultsTo.yourself
		c.er.ReportError(fmt.Errorf("incoming call: results destination is not caller"))

		c.sendMessage(ctx, func(m rpccp.Message) error {
			defer releaseCall()

			mm, err := m.NewUnimplemented()
			if err != nil {
				return rpcerr.Annotatef(err, "incoming call: send unimplemented")
			}

			if err = mm.SetCall(call); err != nil {
				return rpcerr.Annotatef(err, "incoming call: send unimplemented")
			}

			return nil
		}, func(err error) {
			c.er.ReportError(rpcerr.Annotatef(err, "incoming call: send unimplemented"))
		})

		return nil
	}

	c.lk.Lock()
	if c.lk.answers[id] != nil {
		c.lk.Unlock()
		releaseCall()
		return rpcerr.Failedf("incoming call: answer ID %d reused", id)
	}

	var p parsedCall
	parseErr := c.parseCall(&p, call) // parseCall sets CapTable

	// Create return message.
	c.lk.Unlock()
	ret, send, retReleaser, err := c.newReturn()
	if err != nil {
		err = rpcerr.Annotate(err, "incoming call")
		syncutil.With(&c.lk, func() {
			c.lk.answers[id] = errorAnswer(c, id, err)
		})
		c.er.ReportError(err)
		releaseCall()
		return nil
	}
	ret.SetAnswerId(uint32(id))
	ret.SetReleaseParamCaps(false)

	// Find target and start call.
	c.lk.Lock()
	ans := &answer{
		c:           c,
		id:          id,
		ret:         ret,
		sendMsg:     send,
		msgReleaser: retReleaser,
	}
	c.lk.answers[id] = ans
	if parseErr != nil {
		parseErr = rpcerr.Annotate(parseErr, "incoming call")
		ans.sendException(rl, parseErr)
		c.lk.Unlock()
		c.er.ReportError(parseErr)
		releaseCall()
		return nil
	}
	released := false
	releaseArgs := func() {
		if released {
			return
		}
		released = true
		releaseCall()
	}
	switch p.target.which {
	case rpccp.MessageTarget_Which_importedCap:
		ent := c.findExport(p.target.importedCap)
		if ent == nil {
			ans.ret = rpccp.Return{}
			ans.sendMsg = nil
			ans.msgReleaser = nil
			c.lk.Unlock()
			retReleaser.Decr()
			releaseCall()
			return rpcerr.Failedf("incoming call: unknown export ID %d", id)
		}
		c.tasks.Add(1) // will be finished by answer.Return
		var callCtx context.Context
		callCtx, ans.cancel = context.WithCancel(c.bgctx)
		c.lk.Unlock()
		pcall := ent.client.RecvCall(callCtx, capnp.Recv{
			Args:        p.args,
			Method:      p.method,
			ReleaseArgs: releaseArgs,
			Returner:    ans,
		})
		// Place PipelineCaller into answer.  Since the receive goroutine is
		// the only one that uses answer.pcall, it's fine that there's a
		// time gap for this being set.
		ans.setPipelineCaller(p.method, pcall)
		return nil
	case rpccp.MessageTarget_Which_promisedAnswer:
		tgtAns := c.lk.answers[p.target.promisedAnswer]
		if tgtAns == nil || tgtAns.flags.Contains(finishReceived) {
			ans.ret = rpccp.Return{}
			ans.sendMsg = nil
			ans.msgReleaser = nil
			c.lk.Unlock()
			retReleaser.Decr()
			releaseCall()
			return rpcerr.Failedf("incoming call: use of unknown or finished answer ID %d for promised answer target", p.target.promisedAnswer)
		}
		if tgtAns.flags.Contains(resultsReady) {
			if tgtAns.err != nil {
				ans.sendException(rl, tgtAns.err)
				c.lk.Unlock()
				releaseCall()
				return nil
			}
			// tgtAns.results is guaranteed to stay alive because it hasn't
			// received finish yet (it would have been deleted from the
			// answers table), and it can't receive a finish because this is
			// happening on the receive goroutine.
			content, err := tgtAns.results.Content()
			if err != nil {
				err = rpcerr.Failedf("incoming call: read results from target answer: %w", err)
				ans.sendException(rl, err)
				c.lk.Unlock()
				releaseCall()
				c.er.ReportError(err)
				return nil
			}
			sub, err := capnp.Transform(content, p.target.transform)
			if err != nil {
				// Not reporting, as this is the caller's fault.
				ans.sendException(rl, err)
				c.lk.Unlock()
				releaseCall()
				return nil
			}
			iface := sub.Interface()
			var tgt capnp.Client
			switch {
			case sub.IsValid() && !iface.IsValid():
				tgt = capnp.ErrorClient(rpcerr.Failed(ErrNotACapability))
			case !iface.IsValid() || int64(iface.Capability()) >= int64(len(tgtAns.results.Message().CapTable)):
				tgt = capnp.Client{}
			default:
				tgt = tgtAns.results.Message().CapTable[iface.Capability()]
			}
			c.tasks.Add(1) // will be finished by answer.Return
			var callCtx context.Context
			callCtx, ans.cancel = context.WithCancel(c.bgctx)
			c.lk.Unlock()
			pcall := tgt.RecvCall(callCtx, capnp.Recv{
				Args:        p.args,
				Method:      p.method,
				ReleaseArgs: releaseArgs,
				Returner:    ans,
			})
			ans.setPipelineCaller(p.method, pcall)
		} else {
			// Results not ready, use pipeline caller.
			tgtAns.pcalls.Add(1) // will be finished by answer.Return
			var callCtx context.Context
			callCtx, ans.cancel = context.WithCancel(c.bgctx)
			tgt := tgtAns.pcall
			c.tasks.Add(1) // will be finished by answer.Return
			c.lk.Unlock()
			pcall := tgt.PipelineRecv(callCtx, p.target.transform, capnp.Recv{
				Args:        p.args,
				Method:      p.method,
				ReleaseArgs: releaseArgs,
				Returner:    ans,
			})
			tgtAns.pcalls.Done()
			ans.setPipelineCaller(p.method, pcall)
		}
		return nil
	default:
		panic("unreachable")
	}
}

type parsedCall struct {
	target parsedMessageTarget
	method capnp.Method
	args   capnp.Struct
}

type parsedMessageTarget struct {
	which          rpccp.MessageTarget_Which
	importedCap    exportID
	promisedAnswer answerID
	transform      []capnp.PipelineOp
}

func (c *Conn) parseCall(p *parsedCall, call rpccp.Call) error {
	p.method = capnp.Method{
		InterfaceID: call.InterfaceId(),
		MethodID:    call.MethodId(),
	}
	payload, err := call.Params()
	if err != nil {
		return rpcerr.Failedf("read params: %w", err)
	}
	ptr, _, err := c.recvPayload(payload)
	if err != nil {
		return rpcerr.Annotate(err, "read params")
	}
	p.args = ptr.Struct()
	tgt, err := call.Target()
	if err != nil {
		return rpcerr.Failedf("read target: %w", err)
	}
	if err := parseMessageTarget(&p.target, tgt); err != nil {
		return err
	}
	return nil
}

func parseMessageTarget(pt *parsedMessageTarget, tgt rpccp.MessageTarget) error {
	switch pt.which = tgt.Which(); pt.which {
	case rpccp.MessageTarget_Which_importedCap:
		pt.importedCap = exportID(tgt.ImportedCap())
	case rpccp.MessageTarget_Which_promisedAnswer:
		pa, err := tgt.PromisedAnswer()
		if err != nil {
			return rpcerr.Failedf("read target answer: %w", err)
		}
		pt.promisedAnswer = answerID(pa.QuestionId())
		opList, err := pa.Transform()
		if err != nil {
			return rpcerr.Failedf("read target transform: %w", err)
		}
		pt.transform, err = parseTransform(opList)
		if err != nil {
			return rpcerr.Annotate(err, "read target transform")
		}
	default:
		return rpcerr.Unimplementedf("unknown message target %v", pt.which)
	}

	return nil
}

func parseTransform(list rpccp.PromisedAnswer_Op_List) ([]capnp.PipelineOp, error) {
	ops := make([]capnp.PipelineOp, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		li := list.At(i)
		switch li.Which() {
		case rpccp.PromisedAnswer_Op_Which_noop:
			// do nothing
		case rpccp.PromisedAnswer_Op_Which_getPointerField:
			ops = append(ops, capnp.PipelineOp{Field: li.GetPointerField()})
		default:
			return nil, rpcerr.Failedf("transform element %d: unknown type %v", i, li.Which())
		}
	}
	return ops, nil
}

func (c *Conn) handleReturn(ctx context.Context, ret rpccp.Return, release capnp.ReleaseFunc) error {
	c.lk.Lock()
	qid := questionID(ret.AnswerId())
	if uint32(qid) >= uint32(len(c.lk.questions)) {
		c.lk.Unlock()
		release()
		return rpcerr.Failedf("incoming return: question %d does not exist", qid)
	}
	// Pop the question from the table.  Receiving the Return message
	// will always remove the question from the table, because it's the
	// only time the remote vat will use it.
	q := c.lk.questions[qid]
	c.lk.questions[qid] = nil
	if q == nil {
		c.lk.Unlock()
		release()
		return rpcerr.Failedf("incoming return: question %d does not exist", qid)
	}
	canceled := q.flags&finished != 0
	q.flags |= finished
	if canceled {
		// Wait for cancelation task to write the Finish message.  If the
		// Finish message could not be sent to the remote vat, we can't
		// reuse the ID.
		select {
		case <-q.finishMsgSend:
			if q.flags&finishSent != 0 {
				c.lk.questionID.remove(uint32(qid))
			}
			c.lk.Unlock()
			release()
		default:
			c.lk.Unlock()
			release()

			go func() {
				<-q.finishMsgSend
				syncutil.With(&c.lk, func() {
					if q.flags&finishSent != 0 {
						c.lk.questionID.remove(uint32(qid))
					}
				})
			}()
		}
		return nil
	}
	pr := c.parseReturn(ret, q.called) // fills in CapTable
	if pr.parseFailed {
		c.er.ReportError(rpcerr.Annotate(pr.err, "incoming return"))
	}

	if q.bootstrapPromise == nil && pr.err == nil {
		// The result of the message contains actual data (not just a
		// client or an error), so we save the ReleaseFunc for later:
		q.release = release
	}
	c.lk.Unlock()
	// We're going to potentially block fulfilling some promises so fork
	// off a goroutine to avoid blocking the receive loop.
	go func() {
		q.p.Resolve(pr.result, pr.err)
		if q.bootstrapPromise != nil {
			q.bootstrapPromise.Fulfill(q.p.Answer().Client())
			q.p.ReleaseClients()
			// We can release now; root pointer of the result is a client, so the
			// message won't be accessed:
			release()
		} else if pr.err != nil {
			// We can release now; the result is an error, so data from the message
			// won't be accessed:
			release()
		}

		syncutil.With(&c.lk, func() {
			// Send disembargoes.  Failing to send one of these just never lifts
			// the embargo on our side, but doesn't cause a leak.
			//
			// TODO(soon): make embargo resolve to error client.
			for _, s := range pr.disembargoes {
				c.sendMessage(ctx, s.buildDisembargo, func(err error) {
					if err != nil {
						err = fmt.Errorf("incoming return: send disembargo: %w", err)
						c.er.ReportError(err)
					}
				})
			}

			// Send finish.
			c.sendMessage(ctx, func(m rpccp.Message) error {
				fin, err := m.NewFinish()
				if err == nil {
					fin.SetQuestionId(uint32(qid))
					fin.SetReleaseResultCaps(false)
				}
				return err
			}, func(err error) {
				c.lk.Lock()
				defer c.lk.Unlock()
				defer close(q.finishMsgSend)

				if err != nil {
					err = fmt.Errorf("incoming return: send finish: build message: %w", err)
					c.er.ReportError(err)
				} else {
					q.flags |= finishSent
					c.lk.questionID.remove(uint32(qid))
				}
			})
		})
	}()

	return nil
}

func (c *Conn) parseReturn(ret rpccp.Return, called [][]capnp.PipelineOp) parsedReturn {
	switch w := ret.Which(); w {
	case rpccp.Return_Which_results:
		r, err := ret.Results()
		if err != nil {
			return parsedReturn{err: rpcerr.Failedf("parse return: %w", err), parseFailed: true}
		}
		content, locals, err := c.recvPayload(r)
		if err != nil {
			return parsedReturn{err: rpcerr.Failedf("parse return: %w", err), parseFailed: true}
		}

		var embargoCaps uintSet
		var disembargoes []senderLoopback
		mtab := ret.Message().CapTable
		for _, xform := range called {
			p2, _ := capnp.Transform(content, xform)
			iface := p2.Interface()
			if !iface.IsValid() {
				continue
			}
			i := iface.Capability()
			if int64(i) >= int64(len(mtab)) || !locals.has(uint(i)) || embargoCaps.has(uint(i)) {
				continue
			}
			var id embargoID
			id, mtab[i] = c.embargo(mtab[i])
			embargoCaps.add(uint(i))
			disembargoes = append(disembargoes, senderLoopback{
				id:        id,
				question:  questionID(ret.AnswerId()),
				transform: xform,
			})
		}
		return parsedReturn{
			result:       content,
			disembargoes: disembargoes,
		}
	case rpccp.Return_Which_exception:
		e, err := ret.Exception()
		if err != nil {
			return parsedReturn{err: rpcerr.Failedf("parse return: %w", err), parseFailed: true}
		}
		reason, err := e.Reason()
		if err != nil {
			return parsedReturn{err: rpcerr.Failedf("parse return: %w", err), parseFailed: true}
		}
		return parsedReturn{err: exc.New(exc.Type(e.Type()), "", reason)}
	default:
		return parsedReturn{err: rpcerr.Failedf("parse return: unhandled type %v", w), parseFailed: true, unimplemented: true}
	}
}

type parsedReturn struct {
	result        capnp.Ptr
	disembargoes  []senderLoopback
	err           error
	parseFailed   bool
	unimplemented bool
}

func (c *Conn) handleFinish(ctx context.Context, id answerID, releaseResultCaps bool) error {
	rl := &releaseList{}
	defer rl.Release()
	c.lk.Lock()
	defer c.lk.Unlock()

	ans := c.lk.answers[id]
	if ans == nil {
		return rpcerr.Failedf("incoming finish: unknown answer ID %d", id)
	}
	if ans.flags.Contains(finishReceived) {
		return rpcerr.Failedf("incoming finish: answer ID %d already received finish", id)
	}
	ans.flags |= finishReceived
	if releaseResultCaps {
		ans.flags |= releaseResultCapsFlag
	}
	if ans.cancel != nil {
		ans.cancel()
	}
	if !ans.flags.Contains(returnSent) {
		return nil
	}

	// Return sent and finish received: time to destroy answer.
	err := ans.destroy(rl)
	if err != nil {
		return rpcerr.Annotate(err, "incoming finish: release result caps")
	}

	return nil
}

// recvCap materializes a client for a given descriptor.  The caller is
// responsible for ensuring the client gets released.  Any returned
// error indicates a protocol violation.
//
// The caller must be holding onto c.lk.
func (c *Conn) recvCap(d rpccp.CapDescriptor) (capnp.Client, error) {
	switch w := d.Which(); w {
	case rpccp.CapDescriptor_Which_none:
		return capnp.Client{}, nil
	case rpccp.CapDescriptor_Which_senderHosted:
		id := importID(d.SenderHosted())
		return c.addImport(id), nil
	case rpccp.CapDescriptor_Which_senderPromise:
		// We do the same thing as senderHosted, above. @kentonv suggested this on
		// issue #2; this lets messages be delivered properly, although it's a bit
		// of a hack, and as Kenton describes, it has some disadvantages:
		//
		// > * Apps sometimes want to wait for promise resolution, and to find out if
		// >   it resolved to an exception. You won't be able to provide that API. But,
		// >   usually, it isn't needed.
		// > * If the promise resolves to a capability hosted on the receiver,
		// >   messages sent to it will uselessly round-trip over the network
		// >   rather than being delivered locally.
		id := importID(d.SenderPromise())
		return c.addImport(id), nil
	case rpccp.CapDescriptor_Which_receiverHosted:
		id := exportID(d.ReceiverHosted())
		ent := c.findExport(id)
		if ent == nil {
			return capnp.Client{}, rpcerr.Failedf("receive capability: invalid export %d", id)
		}
		return ent.client.AddRef(), nil
	case rpccp.CapDescriptor_Which_receiverAnswer:
		promisedAnswer, err := d.ReceiverAnswer()
		if err != nil {
			return capnp.Client{}, rpcerr.Failedf("receive capability: reading promised answer: %v", err)
		}
		rawTransform, err := promisedAnswer.Transform()
		if err != nil {
			return capnp.Client{}, rpcerr.Failedf("receive capability: reading promised answer transform: %v", err)
		}
		transform, err := parseTransform(rawTransform)
		if err != nil {
			return capnp.Client{}, rpcerr.Failedf("read target transform: %v", err)
		}

		id := answerID(promisedAnswer.QuestionId())
		ans, ok := c.lk.answers[id]
		if !ok {
			return capnp.Client{}, rpcerr.Failedf("receive capability: no such question id: %v", id)
		}

		return c.recvCapReceiverAnswer(ans, transform), nil
	default:
		return capnp.ErrorClient(rpcerr.Failedf("unknown CapDescriptor type %v", w)), nil
	}
}

// Helper for Conn.recvCap(); handles the receiverAnswer case.
func (c *Conn) recvCapReceiverAnswer(ans *answer, transform []capnp.PipelineOp) capnp.Client {
	if ans.promise != nil {
		// Still unresolved.
		future := ans.promise.Answer().Future()
		for _, op := range transform {
			future = future.Field(op.Field, op.DefaultValue)
		}
		return future.Client().AddRef()
	}

	if ans.err != nil {
		return capnp.ErrorClient(ans.err)
	}

	ptr, err := ans.results.Content()
	if err != nil {
		return capnp.ErrorClient(rpcerr.Failedf("except.Failed reading results: %v", err))
	}
	ptr, err = capnp.Transform(ptr, transform)
	if err != nil {
		return capnp.ErrorClient(rpcerr.Failedf("Applying transform to results: %v", err))
	}
	iface := ptr.Interface()
	if !iface.IsValid() {
		return capnp.ErrorClient(rpcerr.Failedf("Result is not a capability"))
	}

	return iface.Client().AddRef()
}

// Returns whether the client should be treated as local, for the purpose of
// embargoes.
func (c *Conn) isLocalClient(client capnp.Client) bool {
	if (client == capnp.Client{}) {
		return false
	}

	bv := client.State().Brand.Value

	if ic, ok := bv.(*importClient); ok {
		// If the connections are different, we must be proxying
		// it, so as far as this connection is concerned, it lives
		// on our side.
		return ic.c != c
	}

	if pc, ok := bv.(capnp.PipelineClient); ok {
		// Same logic re: proxying as with imports:
		if q, ok := c.getAnswerQuestion(pc.Answer()); ok {
			return q.c != c
		}
	}

	if _, ok := bv.(error); ok {
		// Returned by capnp.ErrorClient. No need to treat this as
		// local; all methods will just return the error anyway,
		// so violating E-order will have no effect on the results.
		return false
	}

	return true
}

// recvPayload extracts the content pointer after populating the
// message's capability table.  It also returns the set of indices in
// the capability table that represent capabilities in the local vat.
//
// The caller must be holding onto c.lk.
func (c *Conn) recvPayload(payload rpccp.Payload) (_ capnp.Ptr, locals uintSet, _ error) {
	if !payload.IsValid() {
		// null pointer; in this case we can treat the cap table as being empty
		// and just return.
		return capnp.Ptr{}, nil, nil
	}
	if payload.Message().CapTable != nil {
		// RecvMessage likely violated its invariant.
		return capnp.Ptr{}, nil, rpcerr.Failedf("read payload: %w", ErrCapTablePopulated)
	}
	p, err := payload.Content()
	if err != nil {
		return capnp.Ptr{}, nil, rpcerr.Failedf("read payload: %w", err)
	}
	ptab, err := payload.CapTable()
	if err != nil {
		// Don't allow unreadable capability table to stop other results,
		// just present an empty capability table.
		c.er.ReportError(fmt.Errorf("read payload: capability table: %w", err))
		return p, nil, nil
	}
	mtab := make([]capnp.Client, ptab.Len())
	for i := 0; i < ptab.Len(); i++ {
		var err error
		mtab[i], err = c.recvCap(ptab.At(i))
		if err != nil {
			// FIXME: is it safe to release these while holding c.lk? In general,
			// it's possible for Client.Release() to deadlock.
			for _, client := range mtab[:i] {
				client.Release()
			}
			return capnp.Ptr{}, nil, rpcerr.Annotate(err, fmt.Sprintf("read payload: capability %d", i))
		}
		if c.isLocalClient(mtab[i]) {
			locals.add(uint(i))
		}
	}
	payload.Message().CapTable = mtab
	return p, locals, nil
}

func (c *Conn) handleRelease(ctx context.Context, id exportID, count uint32) error {
	var (
		client capnp.Client
		err    error
	)
	syncutil.With(&c.lk, func() {
		client, err = c.releaseExport(id, count)
	})
	if err != nil {
		return rpcerr.Annotate(err, "incoming release")
	}
	client.Release() // no-ops for nil
	return nil
}

func (c *Conn) handleDisembargo(ctx context.Context, d rpccp.Disembargo, release capnp.ReleaseFunc) error {
	dtarget, err := d.Target()
	if err != nil {
		release()
		return rpcerr.Failedf("incoming disembargo: read target: %w", err)
	}

	var tgt parsedMessageTarget
	if err := parseMessageTarget(&tgt, dtarget); err != nil {
		release()
		return rpcerr.Annotate(err, "incoming disembargo")
	}

	switch d.Context().Which() {
	case rpccp.Disembargo_context_Which_receiverLoopback:
		defer release()

		id := embargoID(d.Context().ReceiverLoopback())
		c.lk.Lock()
		e := c.findEmbargo(id)
		if e == nil {
			c.lk.Unlock()
			return rpcerr.Failedf("incoming disembargo: received sender loopback for unknown ID %d", id)
		}
		// TODO(soon): verify target matches the right import.
		c.lk.embargoes[id] = nil
		c.lk.embargoID.remove(uint32(id))
		c.lk.Unlock()
		e.lift()

	case rpccp.Disembargo_context_Which_senderLoopback:
		var (
			imp    *importClient
			client capnp.Client
		)

		syncutil.With(&c.lk, func() {
			if tgt.which != rpccp.MessageTarget_Which_promisedAnswer {
				err = rpcerr.Failedf("incoming disembargo: sender loopback: target is not a promised answer")
				return
			}

			ans := c.lk.answers[tgt.promisedAnswer]
			if ans == nil {
				err = rpcerr.Failedf("incoming disembargo: unknown answer ID %d", tgt.promisedAnswer)
				return
			}
			if !ans.flags.Contains(returnSent) {
				err = rpcerr.Failedf("incoming disembargo: answer ID %d has not sent return", tgt.promisedAnswer)
				return
			}

			if ans.err != nil {
				err = rpcerr.Failedf("incoming disembargo: answer ID %d returned exception", tgt.promisedAnswer)
				return
			}

			var content capnp.Ptr
			if content, err = ans.results.Content(); err != nil {
				err = rpcerr.Failedf("incoming disembargo: read answer ID %d: %v", tgt.promisedAnswer, err)
				return
			}

			var ptr capnp.Ptr
			if ptr, err = capnp.Transform(content, tgt.transform); err != nil {
				err = rpcerr.Failedf("incoming disembargo: read answer ID %d: %v", tgt.promisedAnswer, err)
				return
			}

			iface := ptr.Interface()
			if !iface.IsValid() || int64(iface.Capability()) >= int64(len(ans.results.Message().CapTable)) {
				err = rpcerr.Failedf("incoming disembargo: sender loopback requested on a capability that is not an import")
				return
			}

			client = iface.Client()
		})

		if err != nil {
			release()
			return err
		}

		imp, ok := client.State().Brand.Value.(*importClient)
		if !ok || imp.c != c {
			client.Release()
			return rpcerr.Failedf("incoming disembargo: sender loopback requested on a capability that is not an import")
		}
		// TODO(maybe): check generation?

		// Since this Cap'n Proto RPC implementation does not send imports
		// unless they are fully dequeued, we can just immediately loop back.
		id := d.Context().SenderLoopback()
		syncutil.With(&c.lk, func() {
			c.sendMessage(ctx, func(m rpccp.Message) error {
				d, err := m.NewDisembargo()
				if err != nil {
					return err
				}

				tgt, err := d.NewTarget()
				if err != nil {
					return err
				}

				tgt.SetImportedCap(uint32(imp.id))
				d.Context().SetReceiverLoopback(id)
				return nil

			}, func(err error) {
				defer release()
				defer client.Release()
				c.er.ReportError(rpcerr.Annotatef(err, "incoming disembargo: send receiver loopback"))
			})
		})

	default:
		c.er.ReportError(fmt.Errorf("incoming disembargo: context %v not implemented", d.Context().Which()))
		syncutil.With(&c.lk, func() {
			c.sendMessage(ctx, func(m rpccp.Message) (err error) {
				if m, err = m.NewUnimplemented(); err == nil {
					err = m.SetDisembargo(d)
				}

				return
			}, func(err error) {
				defer release()
				c.er.ReportError(rpcerr.Annotate(err, "incoming disembargo: send unimplemented"))
			})
		})
	}

	return nil
}

// startTask increments c.tasks if c is not shutting down.
// It returns whether c.tasks was incremented.
//
// The caller must be holding onto c.lk.
func (c *Conn) startTask() (ok bool) {
	if c.bgctx.Err() == nil {
		c.tasks.Add(1)
		ok = true
	}

	return
}

// sendMessage creates a new message on the transport, calls build to
// populate its fields, and enqueues it on the outbound queue.
// When build returns, the message MUST have a nil cap table.
//
// If onSent != nil, it will be called by the send gouroutine
// with the error value returned by the send operation.  If this
// error is nil, the message was successfully sent.
//
// The caller MUST hold c.lk.  onSent will be called without
// holding c.lk.  Callers of sendMessage MAY wish to reacquire the
// c.lk within the onSent.
func (c *Conn) sendMessage(ctx context.Context, build func(rpccp.Message) error, onSent func(error)) {
	outMsg, err := c.transport.NewMessage()
	send := outMsg.Send
	release := outMsg.Release

	// If errors happen when allocating or building the message, set up dummy send/release
	// functions so the error handling logic in onSent() runs as normal:
	if err != nil {
		release = func() {}
		send = func() error {
			return rpcerr.Failedf("create message: %w", err)
		}
	} else if err = build(outMsg.Message); err != nil {
		send = func() error {
			return rpcerr.Failedf("build message: %w", err)
		}
	}

	oldSend := send
	send = func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return oldSend()
	}

	c.lk.sendTx.Send(asyncSend{
		release: release,
		send:    send,
		onSent:  onSent,
	})
}

// reader reads messages from the transport in a loop, and send them down the
// 'in' channel, until the context is canceled or an error occurs. The first
// time RecvMessage() returns an error, its results will still be sent on the
// channel.
func reader(ctx context.Context, in chan<- incomingMessage, t transport.Transport) {
	for {
		inMsg, err := t.RecvMessage()
		incoming := incomingMessage{
			IncomingMessage: inMsg,
			err:             err,
		}
		select {
		case <-ctx.Done():
			return
		case in <- incoming:
		}
		if err != nil {
			return
		}
	}
}

// An incomingMessage bundles the reutrn values of Transport.RecvMessage.
type incomingMessage struct {
	transport.IncomingMessage
	err error
}

type asyncSend struct {
	send    func() error
	onSent  func(error)
	release capnp.ReleaseFunc
}

func (as asyncSend) Abort(err error) {
	defer as.release()

	if as.onSent != nil {
		as.onSent(rpcerr.Disconnected(err))
	}
}

func (as asyncSend) Send() {
	defer as.release()

	if err := as.send(); as.onSent != nil {
		if err != nil {
			err = rpcerr.Failedf("send message: %w", err)
		}

		as.onSent(err)
	}
}
