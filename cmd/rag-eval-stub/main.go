package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/multiagenteval"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "loopback listen address")
	flag.Parse()
	if !strings.HasPrefix(*addr, "127.0.0.1:") && !strings.HasPrefix(*addr, "localhost:") {
		fmt.Fprintln(os.Stderr, "rag-eval-stub only permits a loopback listen address")
		os.Exit(2)
	}
	server := &http.Server{Addr: *addr, Handler: multiagenteval.RAGStubHandler(), ReadHeaderTimeout: 5 * time.Second}
	fmt.Printf("rag eval stub listening on %s token=%s\n", *addr, multiagenteval.RAGStubToken)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
