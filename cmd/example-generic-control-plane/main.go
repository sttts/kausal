// Command example-generic-control-plane demonstrates embedding kausality
// in a generic Kubernetes-style API server using k8s.io/apiserver.
//
// This example uses kcp-dev/embeddedetcd for storage and hardcodes a policy
// that tracks all resources in enforce mode.
//
// Usage:
//
//	go run . --data-dir=/tmp/example-control-plane
//
// For a full implementation, see:
//   - github.com/kcp-dev/generic-controlplane - Complete generic control plane
//   - github.com/kubernetes/sample-apiserver - Kubernetes sample apiserver
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/kcp-dev/embeddedetcd"
	"github.com/kcp-dev/embeddedetcd/options"

	genericoptions "k8s.io/apiserver/pkg/server/options"

	kausalityv1alpha1 "github.com/kausality-io/kausality/api/v1alpha1"
	"github.com/kausality-io/kausality/cmd/example-generic-control-plane/pkg/apiserver"
	"github.com/kausality-io/kausality/pkg/policy"
)

func main() {
	var dataDir string
	var bindAddress string
	var bindPort int

	flag.StringVar(&dataDir, "data-dir", "/tmp/example-control-plane", "Data directory for etcd and server state")
	flag.StringVar(&bindAddress, "bind-address", "127.0.0.1", "Address to bind the API server")
	flag.IntVar(&bindPort, "bind-port", 8443, "Port to bind the API server")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	log := zap.New(zap.UseFlagOptions(&opts))

	log.Info("starting example-generic-control-plane",
		"dataDir", dataDir,
		"bindAddress", bindAddress,
		"bindPort", bindPort,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Create in-memory policy resolver that enforces all resources
	policyResolver := policy.NewStaticResolver(kausalityv1alpha1.ModeEnforce)
	log.Info("policy resolver created", "mode", kausalityv1alpha1.ModeEnforce)

	// Start server with embedded etcd
	if err := run(ctx, log, dataDir, bindAddress, bindPort, policyResolver); err != nil {
		log.Error(err, "server failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, log logr.Logger, dataDir, bindAddress string, bindPort int, policyResolver policy.Resolver) error {
	log.Info("starting embedded etcd server")

	// Create embedded etcd options with root directory
	etcdOpts := options.NewOptions(dataDir)
	etcdOpts.Enabled = true

	// Create standard EtcdOptions for completion
	genericEtcdOpts := genericoptions.NewEtcdOptions(nil)
	genericEtcdOpts.StorageConfig.Transport.ServerList = []string{"embedded"}

	// Complete the embedded etcd options
	completedEtcdOpts := etcdOpts.Complete(genericEtcdOpts)

	// Create etcd config
	etcdConfig, err := embeddedetcd.NewConfig(completedEtcdOpts, true)
	if err != nil {
		return fmt.Errorf("failed to create etcd config: %w", err)
	}

	// Start etcd server
	completedConfig := etcdConfig.Complete()
	etcdServer := embeddedetcd.NewServer(completedConfig)
	if etcdServer == nil {
		return fmt.Errorf("failed to create etcd server")
	}

	// Run etcd server in background
	etcdDone := make(chan struct{})
	go func() {
		defer close(etcdDone)
		if err := etcdServer.Run(ctx); err != nil {
			log.Error(err, "embedded etcd server error")
		}
	}()

	// Wait for etcd to be ready
	log.Info("waiting for embedded etcd to be ready")
	select {
	case <-time.After(60 * time.Second):
		return fmt.Errorf("etcd server startup timed out")
	case <-etcdDone:
		return fmt.Errorf("etcd server exited unexpectedly")
	case <-ctx.Done():
		return ctx.Err()
	default:
		// Give etcd some time to start
		time.Sleep(2 * time.Second)
	}

	log.Info("embedded etcd started successfully")

	// Embedded etcd listens on localhost:2379 by default
	etcdServers := []string{"http://localhost:2379"}
	log.Info("etcd endpoints", "endpoints", etcdServers)

	// Create and start the API server
	server, err := apiserver.New(apiserver.Config{
		EtcdServers:    etcdServers,
		BindAddress:    bindAddress,
		BindPort:       bindPort,
		Log:            log,
		PolicyResolver: policyResolver,
		Client:         nil, // No client needed for simple example
	})
	if err != nil {
		return fmt.Errorf("failed to create API server: %w", err)
	}

	log.Info("API server created, starting...")

	// Run API server in background
	apiDone := make(chan error, 1)
	go func() {
		apiDone <- server.Run(ctx)
	}()

	log.Info("example server running",
		"address", fmt.Sprintf("https://%s:%d", bindAddress, bindPort),
		"apis", "example.kausality.io/v1alpha1",
	)

	// Wait for shutdown
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-apiDone:
		if err != nil {
			log.Error(err, "API server error")
		}
	}

	// Wait for etcd to shut down
	select {
	case <-etcdDone:
	case <-time.After(5 * time.Second):
		log.Info("etcd shutdown timed out")
	}

	return nil
}
