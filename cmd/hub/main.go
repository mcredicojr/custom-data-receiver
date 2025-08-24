package main

import (
	""fmt""
	""log""
	""net/http""
)

func main() {
	mux := http.NewServeMux()

	// Liveness
	mux.HandleFunc(""/healthz"", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(""ok""))
	})

	// Placeholder Rekor webhook endpoint
	mux.HandleFunc(""/webhooks/rekor/"", func(w http.ResponseWriter, r *http.Request) {
		log.Println(""Received Rekor webhook"")
		w.Header().Set(""Content-Type"", ""application/json"")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte({""status"":""received""}))
	})

	addr := ""127.0.0.1:8080""
	fmt.Println(""Custom Data Receiver listening on"", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}