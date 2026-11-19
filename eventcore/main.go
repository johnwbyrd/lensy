// Command eventcore is the durable record of every alert's life and the API
// perception and the operator console talk to. It is deliberately boring: an
// append-only log, a projection rebuilt from it, and a state machine.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	data := flag.String("data", "./data", "data directory (event log and evidence)")
	static := flag.String("static", "../console/dist", "operator console static files ('' to disable)")
	staleAfter := flag.Duration("stale-after", 5*time.Second, "feed heartbeat silence before a feed is declared stale")
	escalateAfter := flag.Duration("escalate-after", 60*time.Second, "unacknowledged time before an alert is escalated")
	flag.Parse()

	lg, err := OpenLog(filepath.Join(*data, "events.jsonl"))
	if err != nil {
		log.Fatalf("open log: %v", err)
	}
	defer lg.Close()
	st := NewState()
	n, err := lg.Replay(st.Apply)
	if err != nil {
		log.Fatalf("replay: %v", err)
	}
	log.Printf("eventcore: replayed %d events from %s", n, filepath.Join(*data, "events.jsonl"))

	core := NewCore(lg, st, *data)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go core.watchdog(ctx, *staleAfter, *escalateAfter)

	srv := &http.Server{Addr: *addr, Handler: core.Routes(*static), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("eventcore: listening on %s (console from %s)", *addr, *static)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
