package main

import (
	"core/internal/api"
	"fmt"
	"net"
	"net/http"
	"os"
	"plugin-framework"
	"plugin-loader/pkg/loader"
	"strconv"
	"sync"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func main() {
	fmt.Println("CORE STARTING...")

	// 1. Configuration
	natsAddr := os.Getenv("NATS_ADDR")
	if natsAddr == "" {
		natsAddr = "127.0.0.1:4222"
	}

	apiAddr := os.Getenv("CORE_API_ADDR")
	if apiAddr == "" {
		apiAddr = "127.0.0.1:49800"
	}

	pluginsDir := os.Getenv("PLUGINS_DIR")
	if pluginsDir == "" {
		pluginsDir = "./plugins"
	}

	// 2. Start Embedded NATS
	host, portStr, _ := net.SplitHostPort(natsAddr)
	port, _ := strconv.Atoi(portStr)
	opts := &natsserver.Options{
		Host:   host,
		Port:   port,
		NoSigs: true,
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		fmt.Printf("NATS Server Error: %v\n", err)
		os.Exit(1)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		fmt.Printf("NATS Server failed to start in time\n")
		os.Exit(1)
	}
	fmt.Printf("NATS Server running on %s\n", ns.Addr().String())

	// 3. Initialize Framework (Optional, core is just a router)
	framework.Init()

	// 4. Shared NATS Client
	nc, err := nats.Connect("nats://" + natsAddr)
	if err != nil {
		fmt.Printf("NATS Client Error: %v\n", err)
		os.Exit(1)
	}

	// 5. Dynamic Bundles (Sidecars)
	go func() {
		loader.LoadPlugins(pluginsDir)
	}()

	// 6. API Server
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		mux := http.NewServeMux()
		s := api.NewServer(nc)
		s.RegisterRoutes(mux)
		fmt.Printf("API Server running on %s\n", apiAddr)
		if err := http.ListenAndServe(apiAddr, mux); err != nil {
			fmt.Printf("API Server Error: %v\n", err)
		}
	}()

	wg.Wait()
}