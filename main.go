package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
	sentrygin "github.com/getsentry/sentry-go/gin"
	sentrysql "github.com/getsentry/sentry-go/sql"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	mwgin "github.com/middleware-labs/golang-apm-gin/gin"
	mwsql "github.com/middleware-labs/golang-apm-sql/sql"
	track "github.com/middleware-labs/golang-apm/tracker"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

var (
	db       *sql.DB
	seeded   bool
	seedMu   sync.Mutex
	mwConfig *track.Config
	apmMode  string
)

func main() {
	apmMode = os.Getenv("APM_MODE")
	if apmMode == "" {
		apmMode = "both"
	}

	initAPM(apmMode)

	if mwConfig != nil {
		defer func() { _ = mwConfig.Tp.Shutdown(context.Background()) }()
	}

	env := os.Getenv("GIN_MODE")
	if env == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	app := gin.Default()

	if mwConfig != nil {
		app.Use(mwgin.Middleware(mwConfig))
	}

	if apmMode == "sentry" || apmMode == "both" {
		app.Use(sentrygin.New(sentrygin.Options{
			Repanic: true,
		}))
	}

	connectDB()

	registerRoutes(app)

	app.Use(gin.CustomRecovery(func(c *gin.Context, recovered interface{}) {
		if hub := sentrygin.GetHubFromContext(c); hub != nil {
			hub.RecoverWithContext(
				context.WithValue(c.Request.Context(), sentry.RequestContextKey, c.Request),
				recovered,
			)
		}
		if mwConfig != nil {
			recordError(c.Request.Context(), fmt.Errorf("panic: %v", recovered), "PanicError")
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal server error"})
	}))

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	printStartupBanner(port)
	log.Fatal(app.Run(":" + port))
}

func connectDB() {
	host := os.Getenv("PG_HOST")
	if host == "" {
		host = "postgres"
	}
	dsn := fmt.Sprintf("host=%s port=5432 user=testuser password=testpass dbname=testdb sslmode=disable", host)

	var err error
	for i := 0; i < 30; i++ {
		if mwConfig != nil {
			db, err = mwsql.Open("postgres", dsn,
				mwsql.WithDBSystem("postgresql"),
				mwsql.WithDBName("testdb"),
			)
		} else if apmMode == "sentry" || apmMode == "both" {
			db, err = sentrysql.Open("postgres", dsn,
				sentrysql.WithDatabaseSystem(sentrysql.SystemPostgreSQL),
				sentrysql.WithDatabaseName("testdb"),
			)
		} else {
			db, err = sql.Open("postgres", dsn)
		}
		if err == nil {
			err = db.Ping()
		}
		if err == nil {
			log.Println("Connected to PostgreSQL")
			db.SetMaxOpenConns(10)
			db.SetMaxIdleConns(5)
			return
		}
		log.Printf("Waiting for DB (%d/30): %v", i+1, err)
		time.Sleep(time.Second)
	}
	log.Fatalf("Could not connect to DB: %v", err)
}

func seedDB() error {
	seedMu.Lock()
	defer seedMu.Unlock()
	if seeded {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS users (
			id SERIAL PRIMARY KEY,
			name VARCHAR(100),
			email VARCHAR(100),
			created_at TIMESTAMP DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS orders (
			id SERIAL PRIMARY KEY,
			user_id INTEGER REFERENCES users(id),
			product VARCHAR(100),
			amount DECIMAL(10,2),
			created_at TIMESTAMP DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS products (
			id SERIAL PRIMARY KEY,
			name VARCHAR(100),
			price DECIMAL(10,2),
			category VARCHAR(50)
		)`,
	} {
		if _, err := tx.Exec(ddl); err != nil {
			return err
		}
	}

	var count int
	tx.QueryRow("SELECT count(*) FROM users").Scan(&count)
	if count == 0 {
		for i := 1; i <= 100; i++ {
			tx.Exec("INSERT INTO users (name, email) VALUES ($1, $2)",
				fmt.Sprintf("User %d", i), fmt.Sprintf("user%d@example.com", i))
		}
		for i := 1; i <= 100; i++ {
			userID := (i % 100) + 1
			tx.Exec("INSERT INTO orders (user_id, product, amount) VALUES ($1, $2, $3)",
				userID, fmt.Sprintf("Product %d", i), rand.Float64()*100)
		}
		cats := []string{"electronics", "clothing", "food"}
		for i := 1; i <= 50; i++ {
			tx.Exec("INSERT INTO products (name, price, category) VALUES ($1, $2, $3)",
				fmt.Sprintf("Product %d", i), rand.Float64()*200, cats[i%3])
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	seeded = true
	log.Println("DB seeded successfully")
	return nil
}

func registerRoutes(app *gin.Engine) {
	app.GET("/health", healthHandler)

	app.GET("/test/n-plus-one", nPlusOneHandler)
	app.GET("/test/delayed-n-plus-one", delayedNPlusOneHandler)
	app.GET("/test/consecutive-db", consecutiveDBHandler)
	app.GET("/test/slow-db", slowDBHandler)
	app.GET("/test/large-payload", largePayloadHandler)
	app.GET("/test/n-plus-one-http", nPlusOneHTTPHandler)
	app.GET("/test/slow-http", slowHTTPHandler)
	app.GET("/test/uncompressed-response", uncompressedResponseHandler)
	app.GET("/test/echo", echoLargeHandler)
	app.GET("/test/large-http-payload", largeHTTPPayloadHandler)
	app.GET("/test/external-http", externalHTTPHandler)
	app.GET("/test/consecutive-http", consecutiveHTTPHandler)
	app.GET("/test/slow-endpoint", slowEndpointHandler)
	app.GET("/test/db-connection-leak", dbConnectionLeakHandler)
	app.GET("/test/long-transaction", longTransactionHandler)
	app.GET("/test/nested-n-plus-one", nestedNPlusOneHandler)
	app.GET("/test/retry-storm", retryStormHandler)
	app.GET("/test/always-fail", alwaysFailHandler)

	app.GET("/test/existing/exception-message", exceptionMessageHandler)
	app.GET("/test/existing/business-error", businessErrorHandler)
	app.GET("/test/existing/high-latency", highLatencyHandler)
	app.GET("/test/existing/log-errors", logErrorsHandler)

	app.GET("/test/all", triggerAllHandler)

	app.GET("/debug-sentry", debugSentryHandler)
	app.GET("/debug-sentry-tracing", debugSentryTracingHandler)
}

// ──────────────────────────────────────────────
// Health check
// ──────────────────────────────────────────────

func healthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "go-sentry-rules-test"})
}

// ──────────────────────────────────────────────
// P1: N+1 Queries
// ──────────────────────────────────────────────

func nPlusOneHandler(c *gin.Context) {
	if err := seedDB(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	count := queryInt(c, "count", 20)
	ctx := c.Request.Context()

	// Single connection to avoid pool-connect spans breaking the sequential pattern
	conn, err := db.Conn(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer conn.Close()

	// The "1" query — get user IDs (source span)
	rows, err := conn.QueryContext(ctx, "SELECT id FROM users LIMIT $1", count)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var ids []int
	for rows.Next() {
		var id int
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()

	// The "+N" queries — fetch each user individually on same connection
	var results []map[string]interface{}
	for _, id := range ids {
		var name, email sql.NullString
		conn.QueryRowContext(ctx,
			"SELECT u.name, u.email, pg_sleep(0.1) FROM users u WHERE u.id = $1", id,
		).Scan(&name, &email, new(string))
		results = append(results, map[string]interface{}{"id": id, "name": name.String, "email": email.String})
	}

	c.JSON(http.StatusOK, gin.H{
		"rule":          "P1 - N+1 Queries",
		"description":   fmt.Sprintf("Fetched %d users individually instead of in one query", count),
		"total_queries": count + 1,
		"fix":           "SELECT * FROM users WHERE id IN (...) with a single query",
		"users":         len(results),
	})
}

// ──────────────────────────────────────────────
// P2: Consecutive DB Queries
// ──────────────────────────────────────────────

// P14: N+1 split across export batches - checks the span buffer's trace assembly.
// The OTel SDK exports spans as they end (batched every ~5s), so queries run gap seconds apart
// reach Kafka / the consumer in different messages, and the root span arrives last. Each batch
// alone (per_batch queries) is below the N+1 minimum of 5 - the issue is only detected when the
// span buffer puts the whole trace back together.
func delayedNPlusOneHandler(c *gin.Context) {
	if err := seedDB(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	batches := queryInt(c, "batches", 2)
	perBatch := queryInt(c, "per_batch", 3)
	gap := time.Duration(queryInt(c, "gap", 8)) * time.Second
	ctx := c.Request.Context()

	// Single connection to avoid pool-connect spans between the queries
	conn, err := db.Conn(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer conn.Close()

	start := time.Now()
	id := 1
	for b := 0; b < batches; b++ {
		if b > 0 {
			time.Sleep(gap) // longer than the SDK export interval - the next spans go in a new batch
		}
		for i := 0; i < perBatch; i++ {
			var name, email sql.NullString
			conn.QueryRowContext(ctx,
				"SELECT u.name, u.email, pg_sleep(0.03) FROM users u WHERE u.id = $1", id,
			).Scan(&name, &email, new(string))
			id++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"rule":        "P14 - N+1 split across export batches (span buffer)",
		"description": fmt.Sprintf("%d batches of %d identical queries, %v apart", batches, perBatch, gap),
		"total_ms":    time.Since(start).Milliseconds(),
		"expect":      "N+1 DB detected only if the span buffer assembles the trace (SB_MAX_DURATION > gap x (batches-1) + export delay)",
	})
}

func consecutiveDBHandler(c *gin.Context) {
	if err := seedDB(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()

	// Single connection to avoid pool-connect spans breaking the sequential pattern
	conn, err := db.Conn(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer conn.Close()

	queries := []string{
		"SELECT count(*), pg_sleep(0.04) FROM users CROSS JOIN orders",
		"SELECT count(*), pg_sleep(0.04) FROM orders CROSS JOIN products",
		"SELECT count(*), pg_sleep(0.04) FROM products CROSS JOIN users",
		"SELECT avg(amount), pg_sleep(0.04) FROM orders CROSS JOIN products",
		"SELECT max(price), pg_sleep(0.04) FROM products CROSS JOIN users",
		"SELECT min(price), pg_sleep(0.04) FROM products CROSS JOIN orders",
	}

	for _, q := range queries {
		conn.ExecContext(ctx, q)
	}

	c.JSON(http.StatusOK, gin.H{
		"rule":        "P2 - Consecutive DB Queries",
		"description": "Ran 6 independent SELECT queries sequentially (no WHERE, no params, each ~40ms+)",
		"fix":         "Run independent queries concurrently with goroutines",
	})
}

// ──────────────────────────────────────────────
// P3: Slow DB Queries
// ──────────────────────────────────────────────

func slowDBHandler(c *gin.Context) {
	if err := seedDB(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Genuinely expensive SELECT (no pg_sleep): hashes users × orders × 300 = 3M rows.
	// Takes ~2.5-3s on postgres:16-alpine with the seeded data (100 users, 100 orders).
	query := "SELECT u.id, u.name, u.email, count(*) AS matches FROM users u CROSS JOIN orders o CROSS JOIN generate_series(1, 300) g WHERE md5(u.email || o.product || g::text) LIKE '00%' GROUP BY u.id, u.name, u.email ORDER BY matches DESC LIMIT 100"

	ctx := c.Request.Context()
	_, span := oteltrace.SpanFromContext(ctx).TracerProvider().
		Tracer("go-test-app").
		Start(ctx, "db.query", oteltrace.WithSpanKind(oteltrace.SpanKindClient))

	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.system.name", "postgresql"),
		attribute.String("db.name", "testdb"),
		attribute.String("db.statement", query),
		attribute.String("db.operation", "SELECT"),
	)

	start := time.Now()
	db.ExecContext(ctx, query)
	duration := time.Since(start).Milliseconds()
	span.End()

	c.JSON(http.StatusOK, gin.H{
		"rule":        "P3 - Slow DB Query",
		"description": "Ran a CPU-heavy SELECT (md5 over users × orders × 300 rows) taking >= 1000ms",
		"query_ms":    duration,
		"threshold":   ">=1000ms, SELECT only, non-truncated db.statement",
	})
}

// ──────────────────────────────────────────────
// P4: Large HTTP Payload
// ──────────────────────────────────────────────

func largePayloadHandler(c *gin.Context) {
	sizeKb := queryInt(c, "size", 400)
	delayMs := queryInt(c, "delay", 150)
	if delayMs > 0 {
		time.Sleep(time.Duration(delayMs) * time.Millisecond)
	}
	items := generateLargePayload(sizeKb)

	payload := gin.H{
		"rule":        "P4 - Large HTTP Payload",
		"description": fmt.Sprintf("Generated ~%dKB response payload", sizeKb),
		"threshold":   "300KB per Sentry default",
		"item_count":  len(items),
		"items":       items,
	}

	encoded, _ := json.Marshal(payload)
	span := oteltrace.SpanFromContext(c.Request.Context())
	span.SetAttributes(
		attribute.String("http.response_content_length", strconv.Itoa(len(encoded))),
	)

	c.Data(http.StatusOK, "application/json", encoded)
}

// ──────────────────────────────────────────────
// P11: N+1 HTTP Calls
// ──────────────────────────────────────────────

func nPlusOneHTTPHandler(c *gin.Context) {
	count := queryInt(c, "count", 15)
	start := time.Now()
	var results []map[string]interface{}

	port := getPort()
	for i := 0; i < count; i++ {
		data, err := tracedGet(c.Request.Context(), fmt.Sprintf("http://localhost:%s/test/slow-endpoint?delay=50&id=%d", port, i))
		if err != nil {
			continue
		}
		results = append(results, data)
	}

	c.JSON(http.StatusOK, gin.H{
		"rule":        "P11 - N+1 HTTP Calls",
		"description": fmt.Sprintf("Made %d sequential identical HTTP GET calls", count),
		"total_ms":    time.Since(start).Milliseconds(),
		"threshold":   "Sentry: 10+ GETs to same host, total > 300ms",
		"fix":         "Batch into a single request or use goroutines",
		"results":     len(results),
	})
}

// ──────────────────────────────────────────────
// P12: Slow Outbound HTTP
// ──────────────────────────────────────────────

func slowHTTPHandler(c *gin.Context) {
	delayMs := queryInt(c, "delay", 2000)
	start := time.Now()

	port := getPort()
	resp, err := http.Get(fmt.Sprintf("http://localhost:%s/test/slow-endpoint?delay=%d", port, delayMs))
	if err == nil {
		resp.Body.Close()
	}

	c.JSON(http.StatusOK, gin.H{
		"rule":        "P12 - Slow Outbound HTTP",
		"description": fmt.Sprintf("Outbound HTTP call took ~%dms", delayMs),
		"duration_ms": time.Since(start).Milliseconds(),
		"threshold":   ">=500ms, analogous to Sentry Slow DB Query",
		"fix":         "Add timeouts, caching, or circuit breakers for slow upstreams",
	})
}

// ──────────────────────────────────────────────
// P13: Uncompressed Response
// ──────────────────────────────────────────────

func uncompressedResponseHandler(c *gin.Context) {
	sizeKb := queryInt(c, "size", 600)
	items := generateLargePayload(sizeKb)

	c.Header("Content-Encoding", "identity")
	c.JSON(http.StatusOK, gin.H{
		"rule":        "P13 - Uncompressed Response",
		"description": fmt.Sprintf("Sent ~%dKB response without compression", sizeKb),
		"threshold":   "Sentry: > 512KB uncompressed, span > 500ms",
		"fix":         "Enable gzip compression middleware",
		"item_count":  len(items),
		"items":       items,
	})
}

// ──────────────────────────────────────────────
// Echo — returns >300KB response for RUM large payload detection
// ──────────────────────────────────────────────

func echoLargeHandler(c *gin.Context) {
	sizeKb := queryInt(c, "size", 350)
	delayMs := queryInt(c, "delay", 150)
	if delayMs > 0 {
		time.Sleep(time.Duration(delayMs) * time.Millisecond)
	}

	items := generateLargePayload(sizeKb)
	encoded, _ := json.Marshal(items)

	c.Header("Content-Length", strconv.Itoa(len(encoded)))
	c.Data(http.StatusOK, "application/json", encoded)
}

// ──────────────────────────────────────────────
// P4b: Large HTTP Payload (outbound, crosses both size + duration thresholds)
// ──────────────────────────────────────────────

func largeHTTPPayloadHandler(c *gin.Context) {
	sizeKb := queryInt(c, "size", 400)
	delayMs := queryInt(c, "delay", 150)
	start := time.Now()

	port := getPort()
	url := fmt.Sprintf("http://localhost:%s/test/large-payload?size=%d&delay=%d", port, sizeKb, delayMs)

	ctx, span := oteltrace.SpanFromContext(c.Request.Context()).TracerProvider().
		Tracer("go-test-app").
		Start(c.Request.Context(), "HTTP GET", oteltrace.WithSpanKind(oteltrace.SpanKindClient))
	defer span.End()

	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	durationMs := time.Since(start).Milliseconds()

	span.SetAttributes(
		attribute.String("http.method", "GET"),
		attribute.String("http.url", url),
		attribute.String("http.status_code", strconv.Itoa(resp.StatusCode)),
		attribute.String("http.response_content_length", strconv.Itoa(len(body))),
	)

	// Also set on the parent server span so detection picks it up regardless of which span it checks
	serverSpan := oteltrace.SpanFromContext(c.Request.Context())
	serverSpan.SetAttributes(
		attribute.String("http.response_content_length", strconv.Itoa(len(body))),
	)

	c.JSON(http.StatusOK, gin.H{
		"rule":             "P4b - Large HTTP Payload (outbound)",
		"description":      fmt.Sprintf("Outbound HTTP call returned ~%dKB in %dms", sizeKb, durationMs),
		"response_bytes":   len(body),
		"duration_ms":      durationMs,
		"threshold_bytes":  300_000,
		"threshold_ms":     100,
		"crossed_size":     len(body) >= 300_000,
		"crossed_duration": durationMs >= 100,
	})
}

// ──────────────────────────────────────────────
// P5: External HTTP call (large payload client-side)
// ──────────────────────────────────────────────

func externalHTTPHandler(c *gin.Context) {
	port := getPort()
	resp, err := http.Get(fmt.Sprintf("http://localhost:%s/test/large-payload?size=400", port))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	c.JSON(http.StatusOK, gin.H{
		"rule":          "P4 - Large HTTP Payload (client-side)",
		"description":   "Made an outbound HTTP call that returned a large payload",
		"response_size": len(body),
	})
}

// ──────────────────────────────────────────────
// P6: Consecutive HTTP Calls
// ──────────────────────────────────────────────

func consecutiveHTTPHandler(c *gin.Context) {
	start := time.Now()
	var results []map[string]interface{}

	// each call must take >= 500ms and the run must save >= 2000ms if parallelised
	delay := queryInt(c, "delay", 600)
	port := getPort()
	for i := 0; i < 5; i++ {
		data, err := tracedGet(c.Request.Context(), fmt.Sprintf("http://localhost:%s/test/slow-endpoint?delay=%d&step=%d", port, delay, i))
		if err != nil {
			continue
		}
		results = append(results, data)
	}

	c.JSON(http.StatusOK, gin.H{
		"rule":        "P6 - Consecutive HTTP Calls",
		"description": "5 sequential HTTP calls that could be parallelized",
		"total_ms":    time.Since(start).Milliseconds(),
		"fix":         "Use goroutines to run independent HTTP calls concurrently",
		"results":     len(results),
	})
}

// tracedGet makes an outbound GET as a client span under ctx, so the call shows up in the
// caller's trace (plain http.Get creates no span and drops the trace context).
func tracedGet(ctx context.Context, url string) (map[string]interface{}, error) {
	ctx, span := oteltrace.SpanFromContext(ctx).TracerProvider().
		Tracer("go-test-app").
		Start(ctx, "GET", oteltrace.WithSpanKind(oteltrace.SpanKindClient))
	defer span.End()

	span.SetAttributes(
		attribute.String("http.request.method", "GET"),
		attribute.String("url.full", url),
		attribute.String("server.address", "localhost"),
	)

	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer resp.Body.Close()
	span.SetAttributes(attribute.String("http.response.status_code", strconv.Itoa(resp.StatusCode)))

	var data map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&data)
	return data, nil
}

func slowEndpointHandler(c *gin.Context) {
	delay := queryInt(c, "delay", 200)
	time.Sleep(time.Duration(delay) * time.Millisecond)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "delayed_ms": delay})
}

// ──────────────────────────────────────────────
// P7: DB Connection Leak
// ──────────────────────────────────────────────

func dbConnectionLeakHandler(c *gin.Context) {
	if err := seedDB(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	leakCount := queryInt(c, "count", 3)
	holdMs := queryInt(c, "hold", 5000)

	ctx := c.Request.Context()
	conns := make([]*sql.Conn, 0, leakCount)
	for i := 0; i < leakCount; i++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			continue
		}
		conn.ExecContext(ctx, "SELECT pg_sleep(0.01)")
		conns = append(conns, conn)
	}

	time.Sleep(time.Duration(holdMs) * time.Millisecond)

	for _, conn := range conns {
		conn.Close()
	}

	c.JSON(http.StatusOK, gin.H{
		"rule":               "P7 - DB Connection Leak",
		"description":        fmt.Sprintf("Held %d connections for %dms without releasing", leakCount, holdMs),
		"connections_leaked": leakCount,
		"hold_ms":            holdMs,
		"note":               "In production, leaked connections exhaust the pool and cause timeouts",
	})
}

// ──────────────────────────────────────────────
// P8: Long Transaction
// ──────────────────────────────────────────────

func longTransactionHandler(c *gin.Context) {
	if err := seedDB(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()
	start := time.Now()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	tx.ExecContext(ctx, "SELECT * FROM users WHERE id = 1 FOR UPDATE")
	tx.ExecContext(ctx, "UPDATE users SET name = $1 WHERE id = 1", fmt.Sprintf("Updated-%d", time.Now().UnixMilli()))
	tx.ExecContext(ctx, "SELECT pg_sleep(0.5)")
	tx.ExecContext(ctx, "UPDATE orders SET amount = amount + 0.01 WHERE user_id = 1")
	tx.ExecContext(ctx, "SELECT pg_sleep(0.3)")
	err = tx.Commit()

	c.JSON(http.StatusOK, gin.H{
		"rule":        "P8 - Long Transaction",
		"description": "Transaction held locks for ~800ms while doing unrelated work",
		"duration_ms": time.Since(start).Milliseconds(),
		"note":        "Long transactions cause lock contention and timeout cascades",
	})
}

// ──────────────────────────────────────────────
// P9: Nested N+1
// ──────────────────────────────────────────────

func nestedNPlusOneHandler(c *gin.Context) {
	if err := seedDB(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()

	// Single connection to avoid pool-connect spans breaking the sequential pattern
	conn, err := db.Conn(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer conn.Close()

	// First N+1: fetch users one by one
	rows, err := conn.QueryContext(ctx, "SELECT id FROM users LIMIT 10")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var userIDs []int
	for rows.Next() {
		var id int
		rows.Scan(&id)
		userIDs = append(userIDs, id)
	}
	rows.Close()

	type userInfo struct{ ID int }
	var users []userInfo
	for _, id := range userIDs {
		var name sql.NullString
		conn.QueryRowContext(ctx,
			"SELECT u.name, pg_sleep(0.05) FROM users u WHERE u.id = $1", id,
		).Scan(&name, new(string))
		users = append(users, userInfo{ID: id})
	}

	// Second N+1: fetch orders for each user
	var orderCount int
	for _, u := range users {
		rows, err := conn.QueryContext(ctx,
			"SELECT o.id, pg_sleep(0.05) FROM orders o WHERE o.user_id = $1", u.ID,
		)
		if err != nil {
			continue
		}
		for rows.Next() {
			orderCount++
		}
		rows.Close()
	}

	c.JSON(http.StatusOK, gin.H{
		"rule":          "P9 - Nested N+1 Queries",
		"description":   "Two separate N+1 patterns in one request: users then orders",
		"user_queries":  len(userIDs) + 1,
		"order_queries": len(users),
		"total_queries": 1 + len(userIDs) + len(users),
		"users":         len(users),
		"orders":        orderCount,
	})
}

// ──────────────────────────────────────────────
// P10: Retry Storm
// ──────────────────────────────────────────────

func retryStormHandler(c *gin.Context) {
	maxRetries := queryInt(c, "retries", 5)
	attempts := 0

	port := getPort()
	for i := 0; i < maxRetries; i++ {
		attempts++
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(fmt.Sprintf("http://localhost:%s/test/always-fail", port))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	c.JSON(http.StatusBadGateway, gin.H{
		"rule":        "P10 - Retry Storm",
		"description": fmt.Sprintf("Made %d retry attempts with no backoff", attempts),
		"attempts":    attempts,
		"note":        "Retry storms amplify outages. Should use exponential backoff + circuit breaker",
	})
}

func alwaysFailHandler(c *gin.Context) {
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Service unavailable"})
}

// ──────────────────────────────────────────────
// M1: Runtime Exception
// ──────────────────────────────────────────────

func exceptionMessageHandler(c *gin.Context) {
	variant := c.DefaultQuery("variant", "null-ref")
	ctx := c.Request.Context()

	var errMsg string
	var errType string

	switch variant {
	case "null-ref":
		errMsg = "runtime error: invalid memory address or nil pointer dereference"
		errType = "NilPointerError"
	case "async-reject":
		errMsg = "Async operation failed: connection reset by peer"
		errType = "ConnectionError"
	case "deep-stack":
		errMsg = "Database connection pool exhausted"
		errType = "PoolExhaustedError"
	case "db-error":
		_, err := db.ExecContext(ctx, "SELECT * FROM nonexistent_table_xyz")
		if err != nil {
			errMsg = err.Error()
			errType = "DatabaseError"
		}
	default:
		errMsg = fmt.Sprintf("Unknown variant: %s", variant)
		errType = "Error"
	}

	simulatedErr := fmt.Errorf("[%s] %s", errType, errMsg)

	if hub := sentrygin.GetHubFromContext(c); hub != nil {
		hub.CaptureException(simulatedErr)
	}

	if mwConfig != nil {
		recordError(ctx, simulatedErr, errType)
	}

	c.JSON(http.StatusInternalServerError, gin.H{
		"rule":         "EXISTING M1 - Runtime Exception",
		"variant":      variant,
		"error_type":   errType,
		"message":      errMsg,
		"how_detected": "sentry.CaptureException + track.ErrorRecording",
	})
}

// ──────────────────────────────────────────────
// M2: Business Errors
// ──────────────────────────────────────────────

func businessErrorHandler(c *gin.Context) {
	scenario := c.DefaultQuery("scenario", "payment")
	ctx := c.Request.Context()

	errors := map[string]string{
		"payment":    "Payment declined: card expired",
		"auth":       "Authentication failed: token expired",
		"validation": "Validation error: email format invalid",
		"timeout":    "Upstream timeout: inventory-service did not respond within 5000ms",
	}

	msg, ok := errors[scenario]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Unknown scenario: %s", scenario)})
		return
	}

	businessErr := fmt.Errorf("[BusinessError] %s", msg)

	if hub := sentrygin.GetHubFromContext(c); hub != nil {
		hub.CaptureException(businessErr)
	}

	if mwConfig != nil {
		recordError(ctx, businessErr, "BusinessError")
	}

	c.JSON(http.StatusInternalServerError, gin.H{
		"rule":         "EXISTING M2 - Business Error",
		"scenario":     scenario,
		"error_type":   "BusinessError",
		"message":      msg,
		"how_detected": "sentry.CaptureException + track.ErrorRecording",
	})
}

// ──────────────────────────────────────────────
// M3: High Tail Latency
// ──────────────────────────────────────────────

func highLatencyHandler(c *gin.Context) {
	delayMs := queryInt(c, "delay", 3000)

	// Latency is app-side only; a slow DB span here would also trip P3 (Slow DB Query).
	time.Sleep(time.Duration(delayMs) * time.Millisecond)

	c.JSON(http.StatusOK, gin.H{
		"rule":        "EXISTING M3 - High Tail Latency",
		"description": fmt.Sprintf("Request took ~%dms (app-side delay)", delayMs),
		"delay_ms":    delayMs,
		"note":        "MW detects via alert threshold on service latency",
	})
}

// ──────────────────────────────────────────────
// M4: Log Errors — uses MW slog integration
// ──────────────────────────────────────────────

func logErrorsHandler(c *gin.Context) {
	count := queryInt(c, "count", 20)

	for i := 0; i < count; i++ {
		slog.Error(fmt.Sprintf("Payment processing failed for order #%d: insufficient funds", 1000+i))
		slog.Error("Failed to connect to upstream service: ECONNREFUSED 10.0.0.5:6379")
		slog.Warn(fmt.Sprintf("Retry attempt %d for upstream connection", i+1))
	}

	c.JSON(http.StatusOK, gin.H{
		"rule":        "EXISTING M4 - Log Error Rate",
		"description": fmt.Sprintf("Emitted %d error logs + %d warning logs via slog", count*2, count),
		"note":        "Uses stdlib slog — logs go to stdout only, not MW APM",
	})
}

// ──────────────────────────────────────────────
// Trigger All
// ──────────────────────────────────────────────

func triggerAllHandler(c *gin.Context) {
	port := getPort()
	baseURL := fmt.Sprintf("http://localhost:%s", port)

	type endpoint struct {
		Name string
		Path string
	}

	endpoints := []endpoint{
		{"n_plus_one", "/test/n-plus-one?count=10"},
		{"consecutive_db", "/test/consecutive-db"},
		{"slow_db", "/test/slow-db"},
		{"large_payload", "/test/large-payload?size=400"},
		{"large_http_payload", "/test/large-http-payload?size=400&delay=150"},
		{"consecutive_http", "/test/consecutive-http"},
		{"long_transaction", "/test/long-transaction"},
		{"nested_n_plus_one", "/test/nested-n-plus-one"},
		{"retry_storm", "/test/retry-storm?retries=3"},
		{"n_plus_one_http", "/test/n-plus-one-http?count=15"},
		{"slow_http", "/test/slow-http?delay=2000"},
		{"uncompressed_response", "/test/uncompressed-response?size=600"},
		{"existing_exception_null_ref", "/test/existing/exception-message?variant=null-ref"},
		{"existing_exception_db", "/test/existing/exception-message?variant=db-error"},
		{"existing_business_payment", "/test/existing/business-error?scenario=payment"},
		{"existing_business_auth", "/test/existing/business-error?scenario=auth"},
		{"existing_high_latency", "/test/existing/high-latency?delay=3000"},
		{"existing_log_errors", "/test/existing/log-errors?count=10"},
	}

	results := make(map[string]interface{})
	for _, ep := range endpoints {
		resp, err := http.Get(baseURL + ep.Path)
		if err != nil {
			results[ep.Name] = gin.H{"status": "error", "rule": err.Error()}
			continue
		}
		var data map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&data)
		resp.Body.Close()
		rule, _ := data["rule"].(string)
		results[ep.Name] = gin.H{"status": resp.StatusCode, "rule": rule}
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "All rules triggered — check OpsAI for issues",
		"results": results,
	})
}

// ──────────────────────────────────────────────
// Debug endpoints
// ──────────────────────────────────────────────

func debugSentryHandler(c *gin.Context) {
	panic("My first Sentry error!")
}

func debugSentryTracingHandler(c *gin.Context) {
	seedDB()
	ctx := c.Request.Context()

	span := sentry.SpanFromContext(ctx)
	hasSpan := span != nil
	var spanName, traceID string
	if span != nil {
		spanName = span.Name
		traceID = span.TraceID.String()
	}

	db.ExecContext(ctx, "SELECT count(*) FROM users")
	db.ExecContext(ctx, "SELECT count(*) FROM orders")
	db.ExecContext(ctx, "SELECT count(*) FROM products")

	c.JSON(http.StatusOK, gin.H{
		"hasActiveSpan": hasSpan,
		"spanName":      spanName,
		"traceId":       traceID,
		"mwConfig":      mwConfig != nil,
		"note":          "Check Sentry/MW Trace view — DB spans should appear as children",
	})
}

// ──────────────────────────────────────────────
// APM Initialization
// ──────────────────────────────────────────────

func initAPM(mode string) {
	if mode == "mw" || mode == "both" {
		apiKey := os.Getenv("MW_API_KEY")
		target := os.Getenv("MW_TARGET")
		serviceName := os.Getenv("MW_SERVICE_NAME")
		if serviceName == "" {
			serviceName = "go-sentry-rules-test"
		}

		if apiKey != "" {
			opts := []track.Options{
				track.WithConfigTag(track.Service, serviceName),
				track.WithConfigTag(track.Token, apiKey),
			}
			if target != "" {
				target = strings.TrimPrefix(target, "https://")
				target = strings.TrimPrefix(target, "http://")
				opts = append(opts, track.WithConfigTag(track.Target, target))
			}

			config, err := track.Track(opts...)
			if err != nil {
				log.Printf("Middleware APM initialization failed: %v", err)
			} else {
				mwConfig = config
				log.Println("Middleware APM initialized")
			}
		} else {
			log.Println("Middleware APM: skipped (no MW_API_KEY)")
		}
	}

	if mode == "sentry" || mode == "both" {
		dsn := os.Getenv("SENTRY_DSN")
		if dsn != "" {
			if err := sentry.Init(sentry.ClientOptions{
				Dsn:              dsn,
				EnableTracing:    true,
				TracesSampleRate: 1.0,
			}); err != nil {
				log.Printf("Sentry initialization failed: %v", err)
			} else {
				log.Println("Sentry initialized")
			}
		}
	}
}

// ──────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────

// recordError adds an exception span event with the correct error type and
// a stack trace starting from the caller's frame (skip=1).
// This replaces track.ErrorRecording which hardcodes exception.type and
// skips too many frames (losing the handler function name).
func recordError(ctx context.Context, err error, errType string) {
	span := oteltrace.SpanFromContext(ctx)
	if span == nil || err == nil {
		return
	}

	// Capture stack from the caller (skip 2: runtime.Callers + recordError)
	pcs := make([]uintptr, 32)
	n := runtime.Callers(2, pcs)
	pcs = pcs[:n]

	var sb strings.Builder
	frames := runtime.CallersFrames(pcs)
	for {
		frame, more := frames.Next()
		fmt.Fprintf(&sb, "%s\n\t%s:%d\n", frame.Function, frame.File, frame.Line)
		if !more {
			break
		}
	}

	if errType == "" {
		errType = reflect.TypeOf(err).String()
	}

	span.AddEvent("exception",
		oteltrace.WithAttributes(
			attribute.String("exception.type", errType),
			attribute.String("exception.message", err.Error()),
			attribute.String("exception.stacktrace", sb.String()),
		),
	)
	span.SetStatus(codes.Error, err.Error())
}

func getPort() string {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	return port
}

func queryInt(c *gin.Context, key string, defaultVal int) int {
	s := c.Query(key)
	if s == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return defaultVal
	}
	return v
}

func generateLargePayload(sizeKb int) []map[string]interface{} {
	itemCount := (sizeKb * 1024) / 200
	items := make([]map[string]interface{}, itemCount)
	for i := 0; i < itemCount; i++ {
		items[i] = map[string]interface{}{
			"id":          i,
			"name":        fmt.Sprintf("Item %d - %s", i, strings.Repeat("x", 100)),
			"description": fmt.Sprintf("Description for item %d with padding %s", i, strings.Repeat("y", 50)),
		}
	}
	return items
}

func printStartupBanner(port string) {
	sentryDSN := os.Getenv("SENTRY_DSN")
	mwKey := os.Getenv("MW_API_KEY")

	sentryStatus := "DISABLED (no SENTRY_DSN)"
	if sentryDSN != "" {
		sentryStatus = "ENABLED"
	}
	mwStatus := "DISABLED (no MW_API_KEY)"
	if mwKey != "" {
		mwStatus = "ENABLED"
	}

	fmt.Printf("Go test app running on port %s\n", port)
	fmt.Printf("Sentry: %s\n", sentryStatus)
	fmt.Printf("Middleware: %s\n", mwStatus)
	fmt.Println()
	fmt.Println("── Performance rules ──")
	fmt.Println("  GET /test/n-plus-one?count=20              → P1:  N+1 DB Queries")
	fmt.Println("  GET /test/consecutive-db                   → P2:  Consecutive DB Queries")
	fmt.Println("  GET /test/slow-db                          → P3:  Slow DB Query")
	fmt.Println("  GET /test/large-payload?size=400           → P4:  Large HTTP Payload")
	fmt.Println("  GET /test/large-http-payload?size=400      → P4b: Large HTTP Payload (outbound)")
	fmt.Println("  GET /test/external-http                    → P5:  Client-side Large Payload")
	fmt.Println("  GET /test/consecutive-http?delay=600       → P6:  Consecutive HTTP Calls")
	fmt.Println("  GET /test/db-connection-leak?count=3       → P7:  DB Connection Leak")
	fmt.Println("  GET /test/long-transaction                 → P8:  Long Transaction")
	fmt.Println("  GET /test/nested-n-plus-one                → P9:  Nested N+1 Queries")
	fmt.Println("  GET /test/retry-storm?retries=5            → P10: Retry Storm")
	fmt.Println("  GET /test/n-plus-one-http?count=15         → P11: N+1 HTTP Calls")
	fmt.Println("  GET /test/slow-http?delay=2000             → P12: Slow Outbound HTTP")
	fmt.Println("  GET /test/uncompressed-response?size=600   → P13: Uncompressed Response")
	fmt.Println("  GET /test/delayed-n-plus-one?gap=8         → P14: N+1 across export batches (span buffer)")
	fmt.Println()
	fmt.Println("── Existing MW detections (baseline) ──")
	fmt.Println("  GET /test/existing/exception-message        → M1: exception.message")
	fmt.Println("      ?variant=null-ref|async-reject|deep-stack|db-error")
	fmt.Println("  GET /test/existing/business-error           → M2: Business errors")
	fmt.Println("      ?scenario=payment|auth|validation|timeout")
	fmt.Println("  GET /test/existing/high-latency?delay=3000  → M3: High Tail Latency")
	fmt.Println("  GET /test/existing/log-errors?count=20      → M4: Log Error Rate")
	fmt.Println()
	fmt.Println("── Debug ──")
	fmt.Println("  GET /debug-sentry                           → Verify Sentry captures")
	fmt.Println("  GET /debug-sentry-tracing                   → Verify DB span instrumentation")
	fmt.Println("  GET /test/all                               → Trigger everything")
}
