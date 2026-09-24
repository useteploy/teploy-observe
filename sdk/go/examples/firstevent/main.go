// Command firstevent is the O11 migration example fixture: credential ->
// first event -> flush -> visible loss counters, exercised end-to-end by
// sdk/e2e/first_event_test.sh against a real Observe server.
//
//	go run ./examples/firstevent -endpoint http://localhost:38080 -api-key obs_...
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	observe "github.com/useteploy/teploy-observe/sdk/go"
)

func main() {
	endpoint := flag.String("endpoint", os.Getenv("OBSERVE_E2E_URL"), "Observe base URL")
	apiKey := flag.String("api-key", os.Getenv("OBSERVE_E2E_KEY"), "site-scoped telemetry API key")
	flag.Parse()

	if *endpoint == "" || *apiKey == "" {
		log.Fatal("firstevent: -endpoint and -api-key (or OBSERVE_E2E_URL/OBSERVE_E2E_KEY) required")
	}
	client, err := observe.New(observe.Options{
		Endpoint:    *endpoint,
		APIKey:      *apiKey,
		SiteID:      "default",
		ServiceName: "o11-first-event",
	})
	if err != nil {
		log.Fatal(err)
	}
	client.Info("o11 go first event", observe.F("source", "sdk-e2e"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Flush(ctx); err != nil {
		log.Fatalf("firstevent: flush: %v (stats: %+v)", err, client.Stats())
	}
	if err := client.Shutdown(ctx); err != nil {
		log.Fatalf("firstevent: shutdown: %v (stats: %+v)", err, client.Stats())
	}
	s := client.Stats()
	if s.DeliveredLogs != 1 || len(s.Dropped) != 0 {
		bs, _ := json.Marshal(s)
		log.Fatalf("firstevent: expected exactly 1 delivered log and no losses, got %s", bs)
	}
	fmt.Println("go-first-event: delivered=1 losses=0")
}
