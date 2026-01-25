package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sigs.k8s.io/yaml"

	kausalityv1alpha1 "github.com/kausality-io/kausality/pkg/callback/v1alpha1"
)

func main() {
	var addr string

	flag.StringVar(&addr, "addr", ":8080", "Address to listen on")
	flag.Parse()

	mux := http.NewServeMux()

	// Webhook endpoint - logs DriftReports as YAML
	mux.HandleFunc("POST /webhook", handleWebhook)

	// Health endpoint
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","time":"%s"}`, time.Now().Format(time.RFC3339))
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Handle shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		fmt.Fprintf(os.Stderr, "kausality-backend-log listening on %s\n", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}

func handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var report kausalityv1alpha1.DriftReport
	if err := json.Unmarshal(body, &report); err != nil {
		http.Error(w, "invalid DriftReport", http.StatusBadRequest)
		return
	}

	// Print as YAML using sigs.k8s.io/yaml which handles RawExtension correctly
	yamlBytes, err := yaml.Marshal(&report)
	if err != nil {
		fmt.Fprintf(os.Stderr, "# failed to marshal: %v\n", err)
	} else {
		fmt.Println("---")
		fmt.Print(string(yamlBytes))
	}

	// Acknowledge
	response := kausalityv1alpha1.DriftReportResponse{Acknowledged: true}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
