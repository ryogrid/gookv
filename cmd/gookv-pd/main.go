// gookv-pd is the Placement Driver server for gookv clusters.
// It provides TSO allocation, cluster metadata management, and region scheduling.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ryogrid/gookv/internal/pd"
)

func main() {
	listenAddr := flag.String("addr", "0.0.0.0:2379", "gRPC listen address")
	dataDir := flag.String("data-dir", "/tmp/gookv-pd", "Data directory for metadata")
	clusterID := flag.Uint64("cluster-id", 1, "Cluster ID")
	flag.Parse()

	cfg := pd.DefaultPDServerConfig()
	cfg.ListenAddr = *listenAddr
	cfg.DataDir = *dataDir
	cfg.ClusterID = *clusterID

	server, err := pd.NewPDServer(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create PD server: %v\n", err)
		os.Exit(1)
	}

	if err := server.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start PD server: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("gookv-pd listening on %s (cluster-id: %d)\n", server.Addr(), *clusterID)

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	fmt.Println("Shutting down PD server...")
	server.Stop()
	fmt.Println("PD server stopped.")
}
