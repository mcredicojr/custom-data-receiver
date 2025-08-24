package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"

	"github.com/mcredicojr/custom-data-receiver/internal/config"
	"github.com/mcredicojr/custom-data-receiver/internal/storage"
)

type RekorNormalized struct {
	EventType  string   `json:"event_type,omitempty"`
	OccurredAt string   `json:"occurred_at,omitempty"`
	CameraID   string   `json:"camera_id,omitempty"`
	CameraName string   `json:"camera_name,omitempty"`
	Plate      string   `json:"plate,omitempty"`
	Confidence float64  `json:"confidence,omitempty"`
	Region     string   `json:"region,omitempty"`
	Lat        *float64 `json:"lat,omitempty"`
	Lon        *float64 `json:"lon,omitempty"`
}

func main() {
	// load config (use example if real one not present)
	cfgPath := "config.yaml"
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		cfgPath = "config.example.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatal("config load:", err)
	}

	// open DB
	var db *storage.DB
	switch cfg.Database.Driver {
	case "sqlite":
		db, err = storage.OpenSQLite(cfg.Database.SQLitePath)
	default:
		log.Fatalf("database driver %q not supported yet", cfg.Database.Driver)
	}
	if err != nil {
		log.Fatal("db open:", err)
	}
	defer db.Close()

	// apply migrations
	migDir := path.Join("internal", "storage", "migrations")
	if err := db.ApplyMigrations(migDir); err != nil {
		log.Fatal("apply migrations:", err)
	}

	mux := http.NewServeMux()

	// Health
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Rekor webhook -> store JSON + normalized fields
	mux.HandleFunc("/webhooks/rekor/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		token := path.Base(r.URL.Path)
		if token == "" || token == "rekor" {
			http.Error(w, "missing token", http.StatusForbidden)
			return
		}
		if !cfg.Providers.Rekor.Enabled || token != cfg.Providers.Rekor.Token {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}

		// idempotency hash
		h := sha256.Sum256(append([]byte("rekor:"), body...))
		hash := hex.EncodeToString(h[:])

		// best-effort normalization (payloads differ)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)

		norm := RekorNormalized{
			EventType: getString(payload, "data_type"),
		}
		if v := getStringDeep(payload, []string{"best_plate", "plate"}); v != "" {
			norm.Plate = v
		} else if v := getStringDeep(payload, []string{"results", "0", "plate"}); v != "" {
			norm.Plate = v
		}
		if c := getFloatDeep(payload, []string{"best_plate", "confidence"}); c != nil {
			norm.Confidence = *c
		}
		if rgn := getStringDeep(payload, []string{"best_plate", "region"}); rgn != "" {
			norm.Region = rgn
		}
		if cam := getString(payload, "camera_name"); cam != "" {
			norm.CameraName = cam
		}
		if ts := getStringDeep(payload, []string{"epoch", "start"}); ts != "" {
			norm.OccurredAt = ts
		}
		norm.Lat = getFloatDeep(payload, []string{"gps", "latitude"})
		norm.Lon = getFloatDeep(payload, []string{"gps", "longitude"})

		// ensure provider row exists
		var providerID int64
		err = db.SQL.QueryRow(`SELECT id FROM providers WHERE key = ?`, "rekor").Scan(&providerID)
		if err != nil {
			res, err2 := db.SQL.Exec(`INSERT INTO providers(key, display_name, status) VALUES (?,?,?)`, "rekor", "Rekor (OpenALPR)", "enabled")
			if err2 != nil {
				http.Error(w, "db error", http.StatusInternalServerError)
				return
			}
			providerID, _ = res.LastInsertId()
		}

		// insert event (ignore if same hash already seen)
		_, err = db.SQL.Exec(`
			INSERT INTO events(
				provider_id, event_type, occurred_at, camera_id, camera_name,
				plate, confidence, region, lat, lon, hash, raw_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(hash) DO NOTHING
		`,
			providerID, nullIfEmpty(norm.EventType), nullIfEmpty(norm.OccurredAt), nullIfEmpty(norm.CameraID),
			nullIfEmpty(norm.CameraName), nullIfEmpty(norm.Plate), nullIfZero(norm.Confidence), nullIfEmpty(norm.Region),
			norm.Lat, norm.Lon, hash, string(body),
		)
		if err != nil {
			http.Error(w, "db insert error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"stored"}`))
	})

	addr := cfg.Server.Addr
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	fmt.Println("Custom Data Receiver listening on", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func getString(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}
func getStringDeep(m map[string]any, path []string) string {
	cur := any(m)
	for _, p := range path {
		switch c := cur.(type) {
		case map[string]any:
			cur = c[p]
		case []any:
			// numeric index string support
			return ""
		default:
			return ""
		}
	}
	if s, ok := cur.(string); ok {
		return s
	}
	return ""
}
func getFloatDeep(m map[string]any, path []string) *float64 {
	cur := any(m)
	for _, p := range path {
		switch c := cur.(type) {
		case map[string]any:
			cur = c[p]
		default:
			return nil
		}
	}
	if f, ok := cur.(float64); ok {
		return &f
	}
	return nil
}
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func nullIfZero(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}
