// Webhook receives signed Raise events and verifies X-Kwork-Signature.
package main

import (
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/21stware/raise-go"
)

func main() {
	secret := os.Getenv("RAISE_WEBHOOK_SECRET")
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		ev, err := raise.VerifyWebhook(body, r.Header.Get("X-Kwork-Signature"), secret, 5*time.Minute)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		log.Printf("event %s %s tenant=%s", ev.Type, ev.ID, ev.Tenant)
		w.WriteHeader(http.StatusOK)
	})
	addr := os.Getenv("RAISE_WEBHOOK_ADDR")
	if addr == "" {
		addr = ":8787"
	}
	log.Fatal(http.ListenAndServe(addr, nil))
}
