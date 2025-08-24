package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"

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

type EventDTO struct {
	ID         int64    `json:"id"`
	Provider   string   `json:"provider"`
	EventType  string   `json:"event_type"`
	OccurredAt *string  `json:"occurred_at"`
	CreatedAt  string   `json:"created_at"`
	CameraID   *string  `json:"camera_id"`
	CameraName *string  `json:"camera_name"`
	Plate      *string  `json:"plate"`
	Confidence *float64 `json:"confidence"`
	Region     *string  `json:"region"`
	Lat        *float64 `json:"lat"`
	Lon        *float64 `json:"lon"`
}

func main() {
	// Load config
	cfgPath := "config.yaml"
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		cfgPath = "config.example.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatal("config load:", err)
	}

	// Open DB
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

	// Apply migrations
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
				log.Println("provider insert error:", err2)
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
			log.Println("event insert error:", err)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}

		jsonOK(w, map[string]string{"status": "stored"})
	})

	// GET /events?page=&page_size=&provider=&plate=&q=
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		page := parseInt(r.URL.Query().Get("page"), 1)
		if page < 1 {
			page = 1
		}
		pageSize := parseInt(r.URL.Query().Get("page_size"), 50)
		if pageSize < 1 || pageSize > 500 {
			pageSize = 50
		}
		offset := (page - 1) * pageSize

		var where []string
		var args []any

		if p := strings.TrimSpace(r.URL.Query().Get("provider")); p != "" {
			where = append(where, "providers.key = ?")
			args = append(args, p)
		}
		if plate := strings.TrimSpace(r.URL.Query().Get("plate")); plate != "" {
			where = append(where, "events.plate LIKE ?")
			args = append(args, "%"+plate+"%")
		}
		if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
			// simple search across plate, camera_name, region
			where = append(where, "(events.plate LIKE ? OR events.camera_name LIKE ? OR events.region LIKE ?)")
			args = append(args, "%"+q+"%", "%"+q+"%", "%"+q+"%")
		}

		sqlStr := `
			SELECT
				events.id,
				providers.key as provider,
				COALESCE(events.event_type,'') as event_type,
				events.occurred_at,
				events.created_at,
				events.camera_id,
				events.camera_name,
				events.plate,
				events.confidence,
				events.region,
				events.lat,
				events.lon
			FROM events
			JOIN providers ON providers.id = events.provider_id
		`
		if len(where) > 0 {
			sqlStr += " WHERE " + strings.Join(where, " AND ")
		}
		sqlStr += " ORDER BY events.created_at DESC LIMIT ? OFFSET ?"
		args = append(args, pageSize, offset)

		rows, err := db.SQL.Query(sqlStr, args...)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		list := make([]EventDTO, 0, pageSize)
		for rows.Next() {
			var e EventDTO
			var (
				occurred sql.NullString
				cameraID sql.NullString
				camName  sql.NullString
				plate    sql.NullString
				conf     sql.NullFloat64
				region   sql.NullString
				lat      sql.NullFloat64
				lon      sql.NullFloat64
			)
			if err := rows.Scan(
				&e.ID, &e.Provider, &e.EventType,
				&occurred, &e.CreatedAt, &cameraID, &camName, &plate, &conf, &region, &lat, &lon,
			); err != nil {
				http.Error(w, "scan error", http.StatusInternalServerError)
				return
			}
			if occurred.Valid {
				e.OccurredAt = &occurred.String
			}
			if cameraID.Valid {
				e.CameraID = &cameraID.String
			}
			if camName.Valid {
				e.CameraName = &camName.String
			}
			if plate.Valid {
				e.Plate = &plate.String
			}
			if conf.Valid {
				v := conf.Float64
				e.Confidence = &v
			}
			if region.Valid {
				e.Region = &region.String
			}
			if lat.Valid {
				v := lat.Float64
				e.Lat = &v
			}
			if lon.Valid {
				v := lon.Float64
				e.Lon = &v
			}
			list = append(list, e)
		}
		jsonOK(w, map[string]any{
			"page":      page,
			"page_size": pageSize,
			"events":    list,
		})
	})

	// /dashboard: minimal HTML page that calls /events
	mux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(dashboardHTML))
	})

	addr := cfg.Server.Addr
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	fmt.Println("Custom Data Receiver listening on", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

func parseInt(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
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

const dashboardHTML = "<!doctype html>\n" +
"<html>\n" +
"<head>\n" +
"  <meta charset=\"utf-8\">\n" +
"  <title>Custom Data Receiver — Dashboard</title>\n" +
"  <meta name=\"viewport\" content=\"width=device-width, initial-scale=1\" />\n" +
"  <style>\n" +
"    body { font-family: system-ui, -apple-system, Segoe UI, Roboto, Arial, sans-serif; margin: 24px; }\n" +
"    h1 { margin: 0 0 16px; }\n" +
"    .controls { display:flex; gap:8px; margin-bottom:12px; align-items:center; flex-wrap:wrap; }\n" +
"    input, select, button { padding:8px; font-size:14px; }\n" +
"    table { border-collapse: collapse; width: 100%; }\n" +
"    th, td { border-bottom: 1px solid #ddd; padding: 8px; text-align: left; }\n" +
"    th { background: #f8f8f8; position: sticky; top: 0; }\n" +
"    .muted { color: #666; font-size: 12px; }\n" +
"  </style>\n" +
"</head>\n" +
"<body>\n" +
"  <h1>Events</h1>\n" +
"  <div class=\"controls\">\n" +
"    <label>Provider\n" +
"      <select id=\"provider\">\n" +
"        <option value=\"\">(all)</option>\n" +
"        <option value=\"rekor\">rekor</option>\n" +
"      </select>\n" +
"    </label>\n" +
"    <input id=\"plate\" placeholder=\"Plate filter (contains)\" />\n" +
"    <input id=\"q\" placeholder=\"Search (plate/camera/region)\" />\n" +
"    <button id=\"refresh\">Refresh</button>\n" +
"  </div>\n" +
"  <div class=\"muted\" id=\"meta\"></div>\n" +
"  <table id=\"tbl\">\n" +
"    <thead>\n" +
"      <tr>\n" +
"        <th>Time (created)</th>\n" +
"        <th>Provider</th>\n" +
"        <th>Event Type</th>\n" +
"        <th>Plate</th>\n" +
"        <th>Conf</th>\n" +
"        <th>Camera</th>\n" +
"        <th>Region</th>\n" +
"        <th>Lat</th>\n" +
"        <th>Lon</th>\n" +
"      </tr>\n" +
"    </thead>\n" +
"    <tbody></tbody>\n" +
"  </table>\n" +
"<script>\n" +
"function esc(x){return (x===null||x===undefined)?'':String(x)}\n" +
"async function load() {\n" +
"  const provider = document.getElementById('provider').value.trim();\n" +
"  const plate = document.getElementById('plate').value.trim();\n" +
"  const q = document.getElementById('q').value.trim();\n" +
"  const params = new URLSearchParams({ page: '1', page_size: '100' });\n" +
"  if (provider) params.set('provider', provider);\n" +
"  if (plate) params.set('plate', plate);\n" +
"  if (q) params.set('q', q);\n" +
"\n" +
"  const res = await fetch('/events?' + params.toString());\n" +
"  const data = await res.json();\n" +
"\n" +
"  document.getElementById('meta').textContent = 'Showing ' + (data.events?.length || 0) + ' event(s) — page ' + data.page + ' of unknown';\n" +
"\n" +
"  const tbody = document.querySelector('#tbl tbody');\n" +
"  tbody.innerHTML = '';\n" +
"  for (const e of (data.events || [])) {\n" +
"    const tr = document.createElement('tr');\n" +
"    tr.innerHTML = '<td>' + esc(e.created_at) + '</td>' +\n" +
"                   '<td>' + esc(e.provider) + '</td>' +\n" +
"                   '<td>' + esc(e.event_type) + '</td>' +\n" +
"                   '<td>' + esc(e.plate) + '</td>' +\n" +
"                   '<td>' + esc(e.confidence) + '</td>' +\n" +
"                   '<td>' + esc(e.camera_name) + '</td>' +\n" +
"                   '<td>' + esc(e.region) + '</td>' +\n" +
"                   '<td>' + esc(e.lat) + '</td>' +\n" +
"                   '<td>' + esc(e.lon) + '</td>';\n" +
"    tbody.appendChild(tr);\n" +
"  }\n" +
"}\n" +
"document.getElementById('refresh').addEventListener('click', load);\n" +
"load();\n" +
"</script>\n" +
"</body>\n" +
"</html>\n"
