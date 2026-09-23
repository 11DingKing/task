package task

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-task/task/v3/internal/checkpoint"
	"github.com/go-task/task/v3/internal/logger"
)

const maxInterruptSignals = 3

// NOTE(@andreynering): This function intercepts SIGINT and SIGTERM signals
// so the Task process is not killed immediately and processes running have
// time to do cleanup work.
//
// The returned context is cancelled when breakpoint mode is enabled and the
// first interrupt is received: running tasks stop at the current node, the
// checkpoint is finalized, and the process can later be continued with
// --resume. Without breakpoint mode the context is never cancelled, so
// existing behavior is preserved.
func (e *Executor) InterceptInterruptSignals(ctx context.Context) context.Context {
	ctx, cancel := context.WithCancelCause(ctx)
	ch := make(chan os.Signal, maxInterruptSignals)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		for i := range maxInterruptSignals {
			sig := <-ch

			if i+1 >= maxInterruptSignals {
				e.Logger.Errf(logger.Red, "task: Signal received for the third time: %q. Forcing shutdown\n", sig)
				os.Exit(1)
			}

			e.Logger.Outf(logger.Yellow, "task: Signal received: %q\n", sig)

			if i == 0 && (e.Breakpoint || e.Resume) {
				e.interrupted.Store(true)
				if e.checkpoint != nil {
					e.checkpoint.MarkInterrupted()
				}
				cancel(checkpoint.ErrInterrupted)
				e.Logger.Outf(logger.Yellow,
					"task: stopping at checkpoint; completed tasks are being saved\n")
			}
		}
	}()

	return ctx
}
