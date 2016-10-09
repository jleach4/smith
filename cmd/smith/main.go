package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/ash2k/smith/pkg/app"
	"github.com/ash2k/smith/pkg/client"
	"github.com/ash2k/smith/pkg/processor"
)

func main() {
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	cancelOnInterrupt(ctx, cancelFunc)

	if err := runApp(ctx); err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		log.Fatalln(err)
	}
}

func runApp(ctx context.Context) error {
	c, err := client.NewInCluster()
	if err != nil {
		return err
	}
	c.Agent = "smith/" + Version + "/" + GitCommit

	allEvents := make(chan interface{})
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()

	watcher := app.NewWatcher(subCtx, c, allEvents)
	defer watcher.Join() // await termination
	defer subCancel()    // cancel ctx to signal done to watcher. If anything below panics, this will be called

	tp := processor.New(subCtx, c)
	defer tp.Join()   // await termination
	defer subCancel() // cancel ctx to signal done to processor (and everything else)

	a := app.App{
		Watcher:   watcher,
		Client:    c,
		Processor: tp,
		Events:    allEvents,
	}
	return a.Run(ctx)
}

// cancelOnInterrupt calls f when os.Interrupt or SIGTERM is received.
// It ignores subsequent interrupts on purpose - program should exit correctly after the first signal.
func cancelOnInterrupt(ctx context.Context, f context.CancelFunc) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-ctx.Done():
		case <-c:
			f()
		}
	}()
}