package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tomRumi/harness-memory-service/internal/embed"
	"github.com/tomRumi/harness-memory-service/internal/index"
	"github.com/tomRumi/harness-memory-service/internal/install"
	"github.com/tomRumi/harness-memory-service/internal/server"
	"github.com/tomRumi/harness-memory-service/internal/store"
)

func cmdServe(p paths, rest []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8765", "listen address; must be a loopback address")
	withVec := fs.Bool("vectors", install.VectorsEnabled(), "use the embedder: vector pass in recall, vectors for new notes (default on; HMS_VECTORS=0 turns it off)")
	fs.Parse(rest)
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		die("serve: %v", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		die("serve: %q is not a loopback address; the service has no authentication and listens on this machine only", *addr)
	}
	d := open(p)
	defer d.Close()
	var e index.Embedder
	if *withVec {
		emb := embed.New(embed.Default())
		hc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if _, err := emb.Health(hc); err != nil {
			fmt.Fprintf(os.Stderr, "hms: vectors unavailable (%v): starting with keyword match only; writes still work, and the service embeds what is missing once the embedder answers\n", err)
		}
		cancel()
		e = emb
	}
	st, err := openStore(p, d, e, true, os.Stderr) // notes left uncommitted or staged by a crash or an earlier failure
	if err != nil {
		die("serve: %v", err)
	}
	defer st.Close()
	srv, err := server.New(st, d, e)
	if err != nil {
		die("serve: %v", err)
	}
	serveHTTP(srv, st, *addr)
}

// serveHTTP serves until SIGINT or SIGTERM, then lets in-flight writes finish and makes the last commit.
func serveHTTP(srv *server.Server, st *store.Store, addr string) {
	hs := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	catchCtx, stopCatch := context.WithCancel(context.Background())
	catchDone := make(chan struct{})
	go func() { // embeds chunks that have no vector, now and every minute
		defer close(catchDone)
		srv.CatchUp(catchCtx, 60*time.Second)
	}()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		stopCatch()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		hs.Shutdown(sctx) // in-flight writes finish first
		cancel()
		st.Commit.Flush()
	}()
	fmt.Printf("hms serving MCP on http://%s/mcp (health: /healthz)\n", addr)
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		stopCatch()
		die("serve: %v", err)
	}
	<-stopped // the last commit is made before the process closes the store
	<-catchDone
}
