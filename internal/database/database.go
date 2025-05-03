package database

import (
	"backend/migrations"
	"context"
	"database/sql"
	"errors" // Import errors package
	"fmt"
	"io/fs"
	"log"
	"os"
	"strconv"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/joho/godotenv/autoload"
	"github.com/pressly/goose/v3"
	_ "gotest.tools/v3/fs"
)

// Service represents a service that interacts with a database.
type Service interface {
	// Health returns a map of health status information.
	Health() map[string]string
	// Close terminates the database connection.
	Close() error
	// GetOrCreateChatUserByHandle finds a user by handle or creates a new one, returning the ID.
	GetOrCreateChatUserByHandle(ctx context.Context, handle string) (int, error)
	// SaveChatLine saves a line of text to a specific chat associated with a user.
	SaveChatLine(ctx context.Context, chatID int, userID int, text string, timestamp time.Time) error
	// EnsureChatExists creates a chat with the given ID if it doesn't exist.
	EnsureChatExists(ctx context.Context, chatID int) error
}

type service struct {
	db *sql.DB
}

var (
	database   = os.Getenv("BLUEPRINT_DB_DATABASE")
	password   = os.Getenv("BLUEPRINT_DB_PASSWORD")
	username   = os.Getenv("BLUEPRINT_DB_USERNAME")
	port       = os.Getenv("BLUEPRINT_DB_PORT")
	host       = os.Getenv("BLUEPRINT_DB_HOST")
	schema     = os.Getenv("BLUEPRINT_DB_SCHEMA")
	dbInstance *service
)

func New() Service {
	// Reuse Connection
	if dbInstance != nil {
		return dbInstance
	}
	connStr := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable&search_path=%s", username, password, host, port, database, schema)
	db, err := sql.Open("pgx", connStr)

	if err != nil {
		log.Fatal(err)
	}

	// Check if the schema exists, create if not
	// Note: This is a basic check. More robust checks might be needed.
	checkSchemaQuery := `SELECT EXISTS(SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`
	var exists bool
	err = db.QueryRow(checkSchemaQuery, schema).Scan(&exists)
	if err != nil {
		log.Fatalf("Failed to check if schema '%s' exists: %v", schema, err)
	}
	if !exists && schema != "" && schema != "public" { // Don't try to create 'public'
		log.Printf("Schema '%s' does not exist. Creating...", schema)
		_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", schema)) // Use IF NOT EXISTS for safety
		if err != nil {
			log.Fatalf("Failed to create schema '%s': %v", schema, err)
		}
		log.Printf("Schema '%s' created successfully.", schema)

		// Re-establish connection string potentially if search_path needs to be re-applied immediately,
		// though usually setting it in the DSN works for the session.
		// Alternatively, set search_path for the session if needed:
		// _, err = db.Exec(fmt.Sprintf("SET search_path TO %s", schema))
		// if err != nil {
		//     log.Printf("Warning: Failed to set search_path for session: %v", err)
		// }
	}

	// Run migrations
	err = MigrateFs(db, migrations.FS, ".")
	if err != nil {
		log.Panicf("Migration error: %w", err) // Changed to %w for error wrapping
	}

	dbInstance = &service{
		db: db,
	}
	return dbInstance
}

// Health checks the health of the database connection by pinging the database.
// It returns a map with keys indicating various health statistics.
func (s *service) Health() map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	stats := make(map[string]string)

	// Ping the database
	err := s.db.PingContext(ctx)
	if err != nil {
		stats["status"] = "down"
		stats["error"] = fmt.Sprintf("db down: %v", err)
		// Removed log.Fatalf here, Health should report status, not terminate
		log.Printf("db down: %v", err)
		return stats
	}

	// Database is up, add more statistics
	stats["status"] = "up"
	stats["message"] = "It's healthy"

	// Get database stats (like open connections, in use, idle, etc.)
	dbStats := s.db.Stats()
	stats["open_connections"] = strconv.Itoa(dbStats.OpenConnections)
	stats["in_use"] = strconv.Itoa(dbStats.InUse)
	stats["idle"] = strconv.Itoa(dbStats.Idle)
	stats["wait_count"] = strconv.FormatInt(dbStats.WaitCount, 10)
	stats["wait_duration"] = dbStats.WaitDuration.String()
	stats["max_idle_closed"] = strconv.FormatInt(dbStats.MaxIdleClosed, 10)
	stats["max_lifetime_closed"] = strconv.FormatInt(dbStats.MaxLifetimeClosed, 10)

	// Evaluate stats to provide a health message (simple examples)
	if dbStats.OpenConnections > 40 { // Assuming 50 is the max for this example
		stats["message"] = "The database is experiencing heavy load."
	} else if dbStats.WaitCount > 1000 {
		stats["message"] = "The database has a high number of wait events."
	} else if dbStats.MaxIdleClosed > 5 || dbStats.MaxLifetimeClosed > 5 { // Example thresholds
		stats["message"] = "Connection pool is cycling connections."
	}

	return stats
}

// EnsureChatExists creates a chat with the given ID if it doesn't exist.
// Uses INSERT ... ON CONFLICT DO NOTHING for atomicity.
func (s *service) EnsureChatExists(ctx context.Context, chatID int) error {
	query := `INSERT INTO chat (id) VALUES ($1) ON CONFLICT (id) DO NOTHING`
	_, err := s.db.ExecContext(ctx, query, chatID)
	if err != nil {
		return fmt.Errorf("failed to ensure chat exists (id %d): %w", chatID, err)
	}
	return nil
}

// GetOrCreateChatUserByHandle finds a user by handle or creates a new one, returning the ID.
// Uses a transaction to ensure atomicity.
func (s *service) GetOrCreateChatUserByHandle(ctx context.Context, handle string) (int, error) {
	var userID int

	// Start transaction
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	// Defer rollback in case of error, commit will override if successful
	defer tx.Rollback()

	// Try to find the user
	selectQuery := `SELECT id FROM chat_user WHERE handle = $1`
	err = tx.QueryRowContext(ctx, selectQuery, handle).Scan(&userID)

	if err == nil {
		// User found, commit transaction early and return ID
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("failed to commit transaction after finding user: %w", err)
		}
		return userID, nil
	} else if errors.Is(err, sql.ErrNoRows) {
		// User not found, insert new user
		insertQuery := `INSERT INTO chat_user (handle) VALUES ($1) RETURNING id`
		err = tx.QueryRowContext(ctx, insertQuery, handle).Scan(&userID)
		if err != nil {
			// Rollback handled by defer
			return 0, fmt.Errorf("failed to insert new chat user '%s': %w", handle, err)
		}
		// Commit transaction after successful insert
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("failed to commit transaction after inserting user: %w", err)
		}
		return userID, nil
	} else {
		// Another error occurred during select query
		// Rollback handled by defer
		return 0, fmt.Errorf("failed to query chat user '%s': %w", handle, err)
	}
}

// SaveChatLine saves a line of text to a specific chat associated with a user.
func (s *service) SaveChatLine(ctx context.Context, chatID int, userID int, text string, timestamp time.Time) error {
	query := `INSERT INTO chat_line (chat_id, user_id, line_text, created_at) VALUES ($1, $2, $3, $4)`
	_, err := s.db.ExecContext(ctx, query, chatID, userID, text, timestamp)
	if err != nil {
		return fmt.Errorf("failed to insert chat line: %w", err)
	}
	return nil
}

// MigrateFs runs migrations from an embedded filesystem.
func MigrateFs(db *sql.DB, migrationFS fs.FS, dir string) error {
	goose.SetBaseFS(migrationFS)
	defer func() {
		goose.SetBaseFS(nil) // Restore original FS
	}()
	return Migrate(db, dir)
}

// Migrate runs migrations using Goose.
func Migrate(db *sql.DB, dir string) error {
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("failed to set goose dialect: %w", err)
	}
	// Ensure the migrations table exists even before running Up
       /* if _, err := goose.EnsureAdminTable(db); err != nil {*/
		/*return fmt.Errorf("failed to ensure goose admin table: %w", err)*/
       /* }*/

	log.Println("Running database migrations...")
	if err := goose.Up(db, dir); err != nil {
		return fmt.Errorf("goose up migration failed: %w", err)
	}
	log.Println("Database migrations completed.")
	return nil
}

// Close closes the database connection.
func (s *service) Close() error {
	log.Printf("Disconnecting from database: %s", database)
	return s.db.Close()
}
