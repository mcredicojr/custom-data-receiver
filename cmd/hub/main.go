package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mcredicojr/custom-data-receiver/internal/config"
	"github.com/mcredicojr/custom-data-receiver/internal/services"
	"github.com/mcredicojr/custom-data-receiver/internal/storage"
)

type RekorNormalized struct {
	EventType  string   `json:"event_type,omitempty"`
	OccurredAt string   `json:"occurred_at,omitempty"` // epoch ms as string if available
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

	// Optional: seed env var from YAML if user placed API key there
	seedEnvFromYAML(cfg)

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

	// Start image worker (does nothing unless jobs are queued)
	ctx, _ := context.WithCancel(context.Background())
	go services.StartImageWorker(ctx, db.SQL, "./data")

	mux := http.NewServeMux()

	// Serve local images read-only from ./data/images at /images/
	imgFS := http.FileServer(http.Dir("./data/images"))
	mux.Handle("/images/", http.StripPrefix("/images/", imgFS))

	// Health
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// DEBUG: show current Rekor image config + whether env is present (redacted)
	mux.HandleFunc("/debug/config", func(w http.ResponseWriter, r *http.Request) {
		type out struct {
			BaseURL      string `json:"base_url"`
			PathTemplate string `json:"path_template"`
			AuthHeader   string `json:"auth_header"`
			HasEnvKey    bool   `json:"has_env_key"`
		}
		ah := cfg.Providers.Rekor.Images.AuthHeader
		redacted := ah
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(ah)), "env ") {
			// show literally what YAML carries (it's not a token itself)
		} else if len(ah) > 12 {
			redacted = ah[:12] + "…redacted…"
		}
		resp := out{
			BaseURL:      cfg.Providers.Rekor.Images.BaseURL,
			PathTemplate: cfg.Providers.Rekor.Images.PathTemplate,
			AuthHeader:   redacted,
			HasEnvKey:    os.Getenv("REKOR_API_KEY") != "",
		}
		jsonOK(w, resp)
	})

	// DEBUG: show recent image jobs and last_error
	mux.HandleFunc("/debug/queue", func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.SQL.Query(`
			SELECT id, type, status, attempts, COALESCE(last_error,''), COALESCE(next_run_at,''), created_at, updated_at, args_json
			  FROM jobs
			 WHERE type='image_fetch'
			 ORDER BY id DESC LIMIT 50`)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		type row struct {
			ID        int64  `json:"id"`
			Type      string `json:"type"`
			Status    string `json:"status"`
			Attempts  int64  `json:"attempts"`
			LastError string `json:"last_error"`
			NextRunAt string `json:"next_run_at"`
			CreatedAt string `json:"created_at"`
			UpdatedAt string `json:"updated_at"`
			ArgsJSON  string `json:"args_json"`
		}
		var out []row
		for rows.Next() {
			var rr row
			_ = rows.Scan(&rr.ID, &rr.Type, &rr.Status, &rr.Attempts, &rr.LastError, &rr.NextRunAt, &rr.CreatedAt, &rr.UpdatedAt, &rr.ArgsJSON)
			out = append(out, rr)
		}
		jsonOK(w, map[string]any{"jobs": out})
	})

	// DEBUG: list images table
	mux.HandleFunc("/debug/images", func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.SQL.Query(`
			SELECT event_id, uuid, COALESCE(path,''), status, created_at, updated_at
			  FROM images
			 ORDER BY id DESC LIMIT 100`)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		type row struct {
			EventID   int64  `json:"event_id"`
			UUID      string `json:"uuid"`
			Path      string `json:"path"`
			Status    string `json:"status"`
			CreatedAt string `json:"created_at"`
			UpdatedAt string `json:"updated_at"`
		}
		var out []row
		for rows.Next() {
			var rr row
			_ = rows.Scan(&rr.EventID, &rr.UUID, &rr.Path, &rr.Status, &rr.CreatedAt, &rr.UpdatedAt)
			out = append(out, rr)
		}
		jsonOK(w, map[string]any{"images": out})
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

		// Parse payload
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)

		// Normalize
		dt := getString(payload, "data_type")
		norm := RekorNormalized{EventType: dt}

		// plate
		if v := getStringDeep(payload, []string{"best_plate", "plate"}); v != "" {
			norm.Plate = v
		} else if v := getStringDeep(payload, []string{"results", "0", "plate"}); v != "" {
			norm.Plate = v
		} else if dt == "alpr_alert" {
			if v := getString(payload, "plate_number"); v != "" {
				norm.Plate = v
			}
		}

		// confidence
		if c := getFloatDeep(payload, []string{"best_plate", "confidence"}); c != nil {
			norm.Confidence = *c
		}

		// region/country
		if rgn := getStringDeep(payload, []string{"best_plate", "region"}); rgn != "" {
			norm.Region = rgn
		} else if rgn := getString(payload, "region"); rgn != "" {
			norm.Region = rgn
		} else if rgn := getString(payload, "country"); rgn != "" {
			norm.Region = rgn
		}

		// camera
		if cam := getString(payload, "camera_name"); cam != "" {
			norm.CameraName = cam
		}
		if cid := getString(payload, "camera_id"); cid != "" {
			norm.CameraID = cid
		} else if cid := getString(payload, "camera_number"); cid != "" {
			norm.CameraID = cid
		}

		// occurred_at (epoch ms as string)
		if dt == "alpr_alert" {
			if ms := getFloatDeep(payload, []string{"epoch_time"}); ms != nil {
				norm.OccurredAt = fmt.Sprintf("%.0f", *ms)
			}
		} else {
			if ms := getFloatDeep(payload, []string{"epoch_end"}); ms != nil {
				norm.OccurredAt = fmt.Sprintf("%.0f", *ms)
			} else if ms := getFloatDeep(payload, []string{"epoch_start"}); ms != nil {
				norm.OccurredAt = fmt.Sprintf("%.0f", *ms)
			}
		}

		// GPS (root or nested)
		if f := getFloatDeep(payload, []string{"gps", "latitude"}); f != nil {
			norm.Lat = f
		} else if f := getFloatDeep(payload, []string{"gps_latitude"}); f != nil {
			norm.Lat = f
		}
		if f := getFloatDeep(payload, []string{"gps", "longitude"}); f != nil {
			norm.Lon = f
		} else if f := getFloatDeep(payload, []string{"gps_longitude"}); f != nil {
			norm.Lon = f
		}

		// alert metadata & group uuids
		var (
			alertList   = getString(payload, "alert_list")
			alertListID = getString(payload, "alert_list_id")
			listType    = getString(payload, "list_type")
			siteName    = getString(payload, "site_name")
			agentUID    = getString(payload, "agent_uid")
			uuids       []string
		)

		if dt == "alpr_alert" {
			if g, ok := payload["group"].(map[string]any); ok {
				// plate fallback from group
				if norm.Plate == "" {
					if v := getStringDeep(g, []string{"best_plate", "plate"}); v != "" {
						norm.Plate = v
					}
				}
				// agent uid fallback from group
				if agentUID == "" {
					if v := getString(g, "agent_uid"); v != "" {
						agentUID = v
					}
				}
				// uuids from group
				if v, ok := g["uuids"].([]any); ok {
					for _, u := range v {
						if s, ok2 := u.(string); ok2 && s != "" {
							uuids = append(uuids, s)
						}
					}
				}
				// GPS fallback from group
				if norm.Lat == nil {
					if f := getFloatDeep(g, []string{"gps_latitude"}); f != nil {
						norm.Lat = f
					}
				}
				if norm.Lon == nil {
					if f := getFloatDeep(g, []string{"gps_longitude"}); f != nil {
						norm.Lon = f
					}
				}
			}
		} else {
			// alpr_group: uuids at top-level
			if v, ok := payload["uuids"].([]any); ok {
				for _, u := range v {
					if s, ok2 := u.(string); ok2 && s != "" {
						uuids = append(uuids, s)
					}
				}
			}
		}

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

		// Look up event id by hash
		var eventID int64
		if err := db.SQL.QueryRow(`SELECT id FROM events WHERE hash=?`, hash).Scan(&eventID); err != nil {
			log.Println("event id lookup error:", err)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}

		// Save alert metadata into event_fields (schema-free)
		saveField := func(key, val string) {
			if val == "" {
				return
			}
			_, _ = db.SQL.Exec(`INSERT INTO event_fields(event_id, key, value) VALUES(?,?,?)`, eventID, key, val)
		}
		if alertList != "" {
			saveField("alert.list", alertList)
		}
		if alertListID != "" {
			saveField("alert.list_id", alertListID)
		}
		if listType != "" {
			saveField("alert.list_type", listType)
		}
		if siteName != "" {
			saveField("alert.site_name", siteName)
		}
		if norm.OccurredAt != "" {
			saveField("occurred_at_ms", norm.OccurredAt)
		}
		if agentUID != "" {
			saveField("agent.uid", agentUID)
		}

		// --- IMAGES FROM THE WEBHOOK (fast path) ---
		if b64 := getString(payload, "image_jpeg"); b64 != "" {
			go saveBase64Image(db.SQL, eventID, b64, "")
		} else if b64 := getString(payload, "img_jpeg"); b64 != "" {
			go saveBase64Image(db.SQL, eventID, b64, "")
		}
		if u := getString(payload, "image_url"); u != "" {
			go saveURLImage(db.SQL, eventID, u, "", "")
		}
		if g, ok := payload["group"].(map[string]any); ok {
			if u := getString(g, "image_url"); u != "" {
				go saveURLImage(db.SQL, eventID, u, "", "")
			}
		}

		// --- UUID-BASED FETCH (fallback; police.openalpr.com requires agent_uid) ---
		for _, uuid := range uuids {
			_, _ = db.SQL.Exec(`INSERT INTO images(event_id, kind, uuid, status) VALUES(?, 'frame', ?, 'pending')`, eventID, uuid)

			if cfg.Providers.Rekor.Images.FetchEnabled && cfg.Providers.Rekor.Images.BaseURL != "" {
				args := services.ImageFetchArgs{
					EventID:      eventID,
					UUID:         uuid,
					BaseURL:      cfg.Providers.Rekor.Images.BaseURL,
					AuthHeader:   cfg.Providers.Rekor.Images.AuthHeader,
					PathTemplate: cfg.Providers.Rekor.Images.PathTemplate, // e.g. /img/{agent_uid}/{uuid}.jpeg?api_key=${REKOR_API_KEY}
					AgentUID:     agentUID,
				}
				b, _ := json.Marshal(args)
				_, _ = db.SQL.Exec(`INSERT INTO jobs(type, args_json, status) VALUES('image_fetch', ?, 'queued')`, string(b))
			}
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

	// API: GET /events/{id}
	mux.HandleFunc("/events/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 2 || parts[0] != "events" {
			http.NotFound(w, r)
			return
		}
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}

		var e EventDTO
		var occurred sql.NullString
		var cameraID, camName, plate, region sql.NullString
		var conf sql.NullFloat64
		var lat, lon sql.NullFloat64

		row := db.SQL.QueryRow(`
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
			WHERE events.id = ?
		`, id)
		if err := row.Scan(&e.ID, &e.Provider, &e.EventType, &occurred, &e.CreatedAt, &cameraID, &camName, &plate, &conf, &region, &lat, &lon); err != nil {
			if err == sql.ErrNoRows {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		if occurred.Valid {
			e.OccurredAt = &occurred.String
		}
		if cameraID.Valid {
			s := cameraID.String
			e.CameraID = &s
		}
		if camName.Valid {
			s := camName.String
			e.CameraName = &s
		}
		if plate.Valid {
			s := plate.String
			e.Plate = &s
		}
		if conf.Valid {
			v := conf.Float64
			e.Confidence = &v
		}
		if region.Valid {
			s := region.String
			e.Region = &s
		}
		if lat.Valid {
			v := lat.Float64
			e.Lat = &v
		}
		if lon.Valid {
			v := lon.Float64
			e.Lon = &v
		}

		// Fields
		fields := []map[string]string{}
		if fr, err := db.SQL.Query(`SELECT key, value FROM event_fields WHERE event_id = ? ORDER BY key`, id); err == nil {
			defer fr.Close()
			for fr.Next() {
				var k, v string
				_ = fr.Scan(&k, &v)
				fields = append(fields, map[string]string{"key": k, "value": v})
			}
		}

		// Images
		images := []map[string]any{}
		if ir, err := db.SQL.Query(`SELECT uuid, COALESCE(path,'') as path, status FROM images WHERE event_id = ? ORDER BY id`, id); err == nil {
			defer ir.Close()
			for ir.Next() {
				var uuid, pathStr, status string
				_ = ir.Scan(&uuid, &pathStr, &status)
				publicPath := ""
				if pathStr != "" {
					publicPath = "/images/" + filepath.Base(pathStr)
				}
				images = append(images, map[string]any{"uuid": uuid, "image_url": publicPath, "status": status})
			}
		}

		jsonOK(w, map[string]any{
			"event":  e,
			"fields": fields,
			"images": images,
		})
	})

	// HTML: /event/{id}
	mux.HandleFunc("/event/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 2 || parts[0] != "event" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, eventHTML)
	})

	// Dashboard
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

// If the YAML contains "auth_header: env REKOR_API_KEY=VALUE",
// set the REKOR_API_KEY environment variable for this process.
// This is ONLY for convenience when OS env isn’t set; it will not
// send that literal value as an HTTP header.
func seedEnvFromYAML(cfg *config.Config) {
	v := strings.TrimSpace(cfg.Providers.Rekor.Images.AuthHeader)
	if strings.HasPrefix(strings.ToLower(v), "env rekor_api_key=") {
		parts := strings.SplitN(v, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[1])
			if key != "" && os.Getenv("REKOR_API_KEY") == "" {
				_ = os.Setenv("REKOR_API_KEY", key)
				log.Println("info: REKOR_API_KEY seeded from YAML for this process")
			}
		}
	}
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

func getStringDeep(m map[string]any, pth []string) string {
	cur := any(m)
	for _, p := range pth {
		switch c := cur.(type) {
		case map[string]any:
			cur = c[p]
		case []any:
			// allow numeric index in path (e.g., "0")
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(c) {
				return ""
			}
			cur = c[i]
		default:
			return ""
		}
	}
	if s, ok := cur.(string); ok {
		return s
	}
	return ""
}

func getFloatDeep(m map[string]any, pth []string) *float64 {
	cur := any(m)
	for _, p := range pth {
		switch c := cur.(type) {
		case map[string]any:
			cur = c[p]
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(c) {
				return nil
			}
			cur = c[i]
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

// --- image helpers (fast path from webhook) ---

func ensureImagesDir() (string, error) {
	root := filepath.Join(".", "data", "images")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return root, nil
}

func saveBase64Image(db *sql.DB, eventID int64, b64 string, filename string) {
	root, err := ensureImagesDir()
	if err != nil {
		log.Printf("images: mkdir error: %v", err)
		return
	}

	// Strip data URI prefix like "data:image/jpeg;base64,..."
	if idx := strings.Index(b64, ","); idx > 0 && strings.Contains(b64[:idx], "base64") {
		b64 = b64[idx+1:]
	}

	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		log.Printf("images: base64 decode error: %v", err)
		return
	}

	if filename == "" {
		sum := sha256.Sum256(data)
		filename = hex.EncodeToString(sum[:]) + ".jpg"
	} else if !strings.HasSuffix(strings.ToLower(filename), ".jpg") && !strings.HasSuffix(strings.ToLower(filename), ".jpeg") {
		filename += ".jpg"
	}

	dst := filepath.Join(root, filepath.Base(filename))
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		log.Printf("images: write error: %v", err)
		return
	}

	_, _ = db.Exec(`INSERT INTO images(event_id, kind, uuid, path, status) VALUES(?, 'frame', '', ?, 'stored')`, eventID, dst)
	log.Printf("images: stored base64 image -> %s", dst)
}

func saveURLImage(db *sql.DB, eventID int64, rawURL, desiredName, authHeader string) {
	root, err := ensureImagesDir()
	if err != nil {
		log.Printf("images: mkdir error: %v", err)
		return
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		log.Printf("images: bad url %q: %v", rawURL, err)
		return
	}

	name := desiredName
	if name == "" {
		base := path.Base(u.Path)
		if base == "" || base == "/" || base == "." {
			base = fmt.Sprintf("img-%d.jpg", time.Now().UnixNano())
		}
		name = base
	}
	dst := filepath.Join(root, filepath.Base(name))

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("images: panic recovering: %v", r)
			}
		}()
		client := &http.Client{Timeout: 8 * time.Second}
		req, _ := http.NewRequest("GET", rawURL, nil)
		if strings.TrimSpace(authHeader) != "" {
			parts := strings.SplitN(authHeader, ":", 2)
			if len(parts) == 2 {
				req.Header.Set(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
			} else {
				req.Header.Set("Authorization", authHeader)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("images: GET %s error: %v", rawURL, err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			log.Printf("images: GET %s -> %d (%s)", rawURL, resp.StatusCode, strings.TrimSpace(string(msg)))
			return
		}
		f, err := os.Create(dst)
		if err != nil {
			log.Printf("images: create %s error: %v", dst, err)
			return
		}
		if _, err := io.Copy(f, resp.Body); err != nil {
			_ = f.Close()
			log.Printf("images: copy error: %v", err)
			return
		}
		_ = f.Close()

		_, _ = db.Exec(`INSERT INTO images(event_id, kind, uuid, path, status) VALUES(?, 'frame', '', ?, 'stored')`, eventID, dst)
		log.Printf("images: stored url image -> %s", dst)
	}()
}

func requireEnv(key string) (string, error) {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return "", errors.New("missing env: " + key)
	}
	return val, nil
}

const dashboardHTML = `<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <title>Custom Data Receiver — Dashboard</title>
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <style>
    body { font-family: system-ui, -apple-system, Segoe UI, Roboto, Arial, sans-serif; margin: 24px; }
    h1 { margin: 0 0 16px; }
    .controls { display:flex; gap:8px; margin-bottom:12px; align-items:center; flex-wrap:wrap; }
    input, select, button { padding:8px; font-size:14px; }
    table { border-collapse: collapse; width: 100%; }
    th, td { border-bottom: 1px solid #ddd; padding: 8px; text-align: left; }
    th { background: #f8f8f8; position: sticky; top: 0; }
    .muted { color: #666; font-size: 12px; }
    .right { text-align:right; }
  </style>
</head>
<body>
  <h1>Events</h1>
  <div class="controls">
    <label>Provider
      <select id="provider">
        <option value="">(all)</option>
        <option value="rekor">rekor</option>
      </select>
    </label>
    <input id="plate" placeholder="Plate filter (contains)" />
    <input id="q" placeholder="Search (plate/camera/region)" />
    <button id="refresh">Refresh</button>
    <span class="muted" id="status"></span>
  </div>
  <div class="muted" id="meta"></div>
  <table id="tbl">
    <thead>
      <tr>
        <th>Time (created)</th>
        <th>Provider</th>
        <th>Event Type</th>
        <th>Plate</th>
        <th class="right">Conf</th>
        <th>Camera</th>
        <th>Region</th>
        <th>Lat</th>
        <th>Lon</th>
      </tr>
    </thead>
    <tbody></tbody>
  </table>
<script>
function esc(x){return (x===null||x===undefined)?'':String(x)}
function fmtLocal(ts){
  try{
    if(!ts) return '';
    if(/^\\d{13}$/.test(String(ts))){ const d=new Date(Number(ts)); if(!isNaN(d)) return d.toLocaleString(); }
    if(/^\\d{4}-\\d{2}-\\d{2} \\d{2}:\\d{2}:\\d{2}(\\.\\d+)?$/.test(ts)){ const d=new Date(ts.replace(' ','T')); if(!isNaN(d)) return d.toLocaleString(); }
    const d=new Date(ts); if(!isNaN(d)) return d.toLocaleString();
    return esc(ts);
  }catch(e){ return esc(ts); }
}
async function load() {
  const provider = document.getElementById('provider').value.trim();
  const plate = document.getElementById('plate').value.trim();
  const q = document.getElementById('q').value.trim();
  const params = new URLSearchParams({ page: '1', page_size: '100' });
  if (provider) params.set('provider', provider);
  if (plate) params.set('plate', plate);
  if (q) params.set('q', q);
  const status = document.getElementById('status');
  status.textContent = 'Loading…';
  const res = await fetch('/events?' + params.toString());
  const data = await res.json();
  status.textContent = 'Last updated ' + new Date().toLocaleTimeString();
  document.getElementById('meta').textContent = 'Showing ' + (data.events?.length || 0) + ' event(s) — page ' + data.page;
  const tbody = document.querySelector('#tbl tbody');
  tbody.innerHTML = '';
  for (const e of (data.events || [])) {
    const tr = document.createElement('tr');
    const conf = (e.confidence==null)?'':Number(e.confidence).toFixed(1);
    const details = '<a href="/event/' + e.id + '">Details</a>';
    tr.innerHTML = '<td>' + fmtLocal(e.created_at) + ' ' + details + '</td>' +
                   '<td>' + esc(e.provider) + '</td>' +
                   '<td>' + esc(e.event_type) + '</td>' +
                   '<td>' + esc(e.plate) + '</td>' +
                   '<td class="right">' + conf + '</td>' +
                   '<td>' + esc(e.camera_name) + '</td>' +
                   '<td>' + esc(e.region) + '</td>' +
                   '<td>' + esc(e.lat) + '</td>' +
                   '<td>' + esc(e.lon) + '</td>';
    tbody.appendChild(tr);
  }
}
document.getElementById('refresh').addEventListener('click', load);
load();
setInterval(load, 10000);
</script>
</body>
</html>`

const eventHTML = `<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Event</title>
  <style>
    body{font-family:system-ui,-apple-system,Segoe UI,Roboto,Arial,sans-serif;margin:24px}
    table{border-collapse:collapse;width:100%}
    th,td{border-bottom:1px solid #ddd;padding:8px;text-align:left}
    th{background:#f8f8f8;position:sticky;top:0}
    .thumb{height:100px}
  </style>
</head>
<body>
<a href="/dashboard">&larr; Back</a>
<h1>Event Details</h1>
<div id="meta"></div>
<h2>Images</h2><div id="imgs"></div>
<h2>Fields</h2>
<table><thead><tr><th>Key</th><th>Value</th></tr></thead><tbody id="fields"></tbody></table>
<script>
function esc(x){return (x===null||x===undefined)?'':String(x)}
async function load(){
  const id = location.pathname.split('/').pop();
  const res = await fetch('/events/'+id);
  const data = await res.json();
  const e = data.event || {};
  document.getElementById('meta').innerHTML =
    '<b>ID</b>: '+esc(e.id)+' &nbsp; <b>Provider</b>: '+esc(e.provider)+' &nbsp; <b>Type</b>: '+esc(e.event_type)+'<br>'+
    '<b>Plate</b>: '+esc(e.plate)+' &nbsp; <b>Camera</b>: '+esc(e.camera_name)+'<br>'+
    '<b>Created</b>: '+esc(e.created_at)+' &nbsp; <b>Occurred</b>: '+esc(e.occurred_at)+'<br>'+
    '<b>Region</b>: '+esc(e.region)+' &nbsp; <b>Conf</b>: '+esc(e.confidence)+' &nbsp; <b>GPS</b>: '+esc(e.lat)+', '+esc(e.lon);
  const imgDiv = document.getElementById('imgs'); imgDiv.innerHTML='';
  (data.images||[]).forEach(img=>{
    const d=document.createElement('div');
    d.style.display='inline-block'; d.style.margin='4px';
    d.innerHTML = (img.image_url?('<img class="thumb" src="'+img.image_url+'"/>'):'(pending)')
                + '<br>'+esc(img.uuid)+'<br><span style="color:#666">'+esc(img.status)+'</span>';
    imgDiv.appendChild(d);
  });
  const tb = document.getElementById('fields'); tb.innerHTML='';
  (data.fields||[]).forEach(f=>{
    const tr=document.createElement('tr');
    tr.innerHTML='<td>'+esc(f.key)+'</td><td>'+esc(f.value)+'</td>';
    tb.appendChild(tr);
  });
}
load();
</script>
</body>
</html>`
