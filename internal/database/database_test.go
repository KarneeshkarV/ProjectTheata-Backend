package database

import (
	"context"
	"database/sql"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"log"
	"testing"
	"time"
)

func mustStartPostgresContainer() (func(context.Context, ...testcontainers.TerminateOption) error, error) {
	var (
		dbName = "database"
		dbPwd  = "password"
		dbUser = "user"
	)

	dbContainer, err := postgres.Run(
		context.Background(),
		"postgres:latest",
		postgres.WithDatabase(dbName),
		postgres.WithUsername(dbUser),
		postgres.WithPassword(dbPwd),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(5*time.Second)),
	)
	if err != nil {
		return nil, err
	}

	database = dbName
	password = dbPwd
	username = dbUser

	dbHost, err := dbContainer.Host(context.Background())
	if err != nil {
		return dbContainer.Terminate, err
	}

	dbPort, err := dbContainer.MappedPort(context.Background(), "5432/tcp")
	if err != nil {
		return dbContainer.Terminate, err
	}

	host = dbHost
	port = dbPort.Port()

	return dbContainer.Terminate, err
}

func TestMain(m *testing.M) {
	teardown, err := mustStartPostgresContainer()
	if err != nil {
		log.Fatalf("could not start postgres container: %v", err)
	}

	m.Run()

	if teardown != nil && teardown(context.Background()) != nil {
		log.Fatalf("could not teardown postgres container: %v", err)
	}
}

func TestNew(t *testing.T) {
	srv := New()
	if srv == nil {
		t.Fatal("New() returned nil")
	}
}

func TestHealth(t *testing.T) {
	srv := New()

	stats := srv.Health()

	if stats["status"] != "up" {
		t.Fatalf("expected status to be up, got %s", stats["status"])
	}

	if _, ok := stats["error"]; ok {
		t.Fatalf("expected error not to be present")
	}

	if stats["message"] != "It's healthy" {
		t.Fatalf("expected message to be 'It's healthy', got %s", stats["message"])
	}
}

func TestClose(t *testing.T) {
	srv := New()

	if srv.Close() != nil {
		t.Fatalf("expected Close() to return nil")
	}
}

//======================================================================================
// Functions to be Tested
//
// In a real application, these would be in their own file (e.g., `database.go`).
// They are included here to make the example self-contained and runnable.
//======================================================================================

// User represents a user entity in our system.
type User struct {
	ID   int
	Name string
}

// GetUser retrieves a user by their ID from the database.
func GetUser(db *sql.DB, id int) (*User, error) {
	// The SQL query to find a user by ID.
	query := "SELECT id, name FROM users WHERE id = ?"
	row := db.QueryRow(query, id)

	var user User
	// Scan the row's results into the User struct.
	if err := row.Scan(&user.ID, &user.Name); err != nil {
		// This will return sql.ErrNoRows if no user is found.
		return nil, err
	}
	return &user, nil
}

// CreateUser adds a new user to the database and returns their new ID.
func CreateUser(db *sql.DB, name string) (int64, error) {
	// The SQL query to insert a new user.
	query := "INSERT INTO users (name) VALUES (?)"
	result, err := db.Exec(query, name)
	if err != nil {
		return 0, err
	}
	// Get the ID of the newly inserted row.
	return result.LastInsertId()
}

//======================================================================================
// Test Suite Definition
//======================================================================================

// DatabaseTestSuite encapsulates the test suite for our database operations.
type DatabaseTestSuite struct {
	suite.Suite
	db   *sql.DB
	mock sqlmock.Sqlmock
}

// SetupTest is executed before each test in the suite.
// It initializes a new sqlmock database connection and the mock controller.
func (s *DatabaseTestSuite) SetupTest() {
	var err error
	// sqlmock.New() creates a new mock database connection and a mock controller.
	s.db, s.mock, err = sqlmock.New()
	// Require().NoError asserts that the mock was created without error.
	s.Require().NoError(err)
}

// TearDownTest is executed after each test in the suite.
// It ensures that the database connection is closed and all expected interactions
// with the mock were performed.
func (s *DatabaseTestSuite) TearDownTest() {
	// We expect the Close method to be called on our database connection.
	s.mock.ExpectClose()
	err := s.db.Close()
	s.Require().NoError(err)
	// s.mock.ExpectationsWereMet() ensures that all expectations we set in our tests were met.
	s.Require().NoError(s.mock.ExpectationsWereMet())
}

// TestDatabaseTestSuite is the entry point for running the test suite.
// The `go test` command will discover this function and run the suite.
func TestDatabaseTestSuite(t *testing.T) {
	suite.Run(t, new(DatabaseTestSuite))
}

//======================================================================================
// Tests for GetUser
//======================================================================================

// TestGetUser_Success tests the successful retrieval of a user.
func (s *DatabaseTestSuite) TestGetUser_Success() {
	// Define the expected user and the rows that should be returned by the mock DB.
	expectedUser := &User{ID: 1, Name: "John Doe"}
	rows := sqlmock.NewRows([]string{"id", "name"}).AddRow(expectedUser.ID, expectedUser.Name)

	// Set up the expectation on the mock. We expect a query that matches the regex.
	// We use a regex for flexibility with whitespace and special characters in SQL.
	query := "SELECT id, name FROM users WHERE id = \\?"
	s.mock.ExpectQuery(query).
		WithArgs(1).         // We expect the query to be called with the argument '1'.
		WillReturnRows(rows) // It should return the predefined rows.

	// Call the function we are testing.
	user, err := GetUser(s.db, 1)

	// Assert that the function returned no error and the correct user.
	s.Require().NoError(err)
	s.Equal(expectedUser, user)
}

// TestGetUser_NotFound tests the case where a user is not found in the database.
func (s *DatabaseTestSuite) TestGetUser_NotFound() {
	// Set up the expectation on the mock.
	query := "SELECT id, name FROM users WHERE id = \\?"
	s.mock.ExpectQuery(query).
		WithArgs(2).                   // We expect a different user ID this time.
		WillReturnError(sql.ErrNoRows) // The mock should return the standard "no rows" error.

	// Call the function and assert that it returns the expected error.
	_, err := GetUser(s.db, 2)
	s.Equal(sql.ErrNoRows, err, "Expected sql.ErrNoRows, got %v", err)
}

//======================================================================================
// Tests for CreateUser
//======================================================================================

// TestCreateUser_Success tests the successful creation of a new user.
func (s *DatabaseTestSuite) TestCreateUser_Success() {
	// Set up the expectation for an Exec command.
	query := "INSERT INTO users \\(name\\) VALUES \\(\\?\\)"
	s.mock.ExpectExec(query).
		WithArgs("Jane Doe").                     // We expect this specific name to be used as an argument.
		WillReturnResult(sqlmock.NewResult(1, 1)) // The mock will return a result indicating 1 new row with LastInsertId=1.

	// Call the function we are testing.
	lastInsertID, err := CreateUser(s.db, "Jane Doe")

	// Assert that there was no error and the returned ID is correct.
	s.Require().NoError(err)
	s.Equal(int64(1), lastInsertID)
}

// TestCreateUser_Failure tests a failed user creation due to a database error.
func (s *DatabaseTestSuite) TestCreateUser_Failure() {
	// Define a custom error for the test.
	dbError := errors.New("something went wrong")

	// Set up the expectation.
	query := "INSERT INTO users \\(name\\) VALUES \\(\\?\\)"
	s.mock.ExpectExec(query).
		WithArgs("Jane Doe").
		WillReturnError(dbError) // The mock will return our custom error.

	// Call the function.
	_, err := CreateUser(s.db, "Jane Doe")

	// Assert that the error returned by the function is the one we defined.
	s.Equal(dbError, err)
}
