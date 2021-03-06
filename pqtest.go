package pqtest

import (
	"database/sql"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/lib/pq"
)

const timeFormat = "20060102150405"

var random = rand.New(rand.NewSource(time.Now().UnixNano()))

// A Fataler provides the ability to immediately fail.
// In the standard library, it's implemented by
// *testing.T, *testing.B and *log.Logger.
type Fataler interface {
	Fatal(...interface{})
}

// An Option allows a caller of Open to customize
// the test database.
type Option interface {
	apply(Fataler, *optionData)
}

type optionData struct {
	schema      []string
	schemaPaths []string
	databaseURL string
}

type optionFn func(Fataler, *optionData)

func (of optionFn) apply(f Fataler, data *optionData) {
	of(f, data)
}

// SchemaFile returns an Option that will initialize the new
// test database the schema at the provided filePath.
func SchemaFile(filePath string) Option {
	return optionFn(func(f Fataler, data *optionData) {
		data.schemaPaths = append(data.schemaPaths, filePath)
	})
}

// Migrations returns an Option that will initialize the new
// test database by applying the contents of each sql file
// in the provided directory, ordered by the filenames.
func Migrations(dir string) Option {
	return optionFn(func(f Fataler, data *optionData) {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if filepath.Ext(path) != ".sql" || info.IsDir() {
				return nil
			}
			data.schemaPaths = append(data.schemaPaths, path)
			return nil
		})
		if err != nil {
			f.Fatal(err)
		}
	})
}

// Open creates a new test PostgreSQL database, returning
// a *sql.DB opened to the database.
//
// Databases created by pqtest are garbage collected by
// subsequent calls to pqtest.Open.
func Open(f Fataler, opts ...Option) *sql.DB {
	data := optionData{
		databaseURL: "postgres:///postgres?sslmode=disable",
	}
	for _, opt := range opts {
		opt.apply(f, &data)
	}

	for _, sp := range data.schemaPaths {
		schemaBytes, err := ioutil.ReadFile(sp)
		if err != nil {
			f.Fatal(sp, err)
		}
		data.schema = append(data.schema, string(schemaBytes))
	}

	newDatabaseURL, err := mkdb(data.databaseURL)
	if err != nil {
		f.Fatal(err)
	}
	fmt.Println(newDatabaseURL)
	db, err := sql.Open("postgres", newDatabaseURL)
	if err != nil {
		f.Fatal(err)
	}
	for _, schema := range data.schema {
		_, err = db.Exec(schema)
		if err != nil {
			f.Fatal(err)
		}
	}
	return db
}

func mkdb(dbURL string) (string, error) {
	_, file, _, _ := runtime.Caller(2)

	name := randomDBName(file)
	u, err := url.Parse(dbURL)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	u.RawPath = "/" + name

	ctldb, err := sql.Open("postgres", dbURL)
	if err != nil {
		return "", err
	}
	defer ctldb.Close()

	err = garbageCollectDBs(ctldb)
	if err != nil {
		return "", err
	}

	_, err = ctldb.Exec("CREATE DATABASE " + pq.QuoteIdentifier(name))
	return u.String(), err
}

func randomDBName(file string) (dbname string) {
	var s string
	const chars = "abcdefghijklmnopqrstuvwxyz"
	for i := 0; i < 10; i++ {
		s += string(chars[random.Intn(len(chars))])
	}
	suffix := s
	withoutExt := strings.TrimSuffix(filepath.Base(file), ".go")
	if withoutExt != "" {
		suffix = suffix + "_" + withoutExt
	}
	return formatDBName(suffix, time.Now())
}

func formatDBName(suffix string, t time.Time) string {
	dbname := "pqtest_" + t.UTC().Format(timeFormat) + "Z_" + suffix
	return dbname
}

func garbageCollectDBs(db *sql.DB) error {
	const gcDur = 3 * time.Minute
	gcTime := time.Now().Add(-gcDur)
	const q = `
		SELECT datname FROM pg_database
		WHERE datname LIKE 'pqtest_%' AND datname < $1
	`
	rows, err := db.Query(q, formatDBName("db", gcTime))
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		err = rows.Scan(&name)
		if err != nil {
			return err
		}
		names = append(names, name)
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	for i, name := range names {
		if i > 5 {
			break // drop up to five databases per test
		}
		go db.Exec("DROP DATABASE " + pq.QuoteIdentifier(name))
	}
	return nil
}
