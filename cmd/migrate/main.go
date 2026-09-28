// Command migrate applies the CQL files in ./migrations to a ScyllaDB keyspace, in file
// name order, and records what it applied in a schema_migrations table so re-running it
// only executes new files.
//
// A keyspace that already has tables but no schema_migrations record (one that was
// migrated by hand with cqlsh before this tool existed) is never touched: some migration
// files are destructive (019 drops and recreates a table), so replaying them would
// destroy data. Such a keyspace must be adopted first with -adopt, which records every
// current file as applied without executing anything; new files added later are then
// applied normally.
//
// Configuration comes from the same SCYLLA_* variables as the API (and the same env file,
// see ENV_FILE), with flags to override:
//
//	go run ./cmd/migrate                       # apply pending migrations
//	go run ./cmd/migrate -dry-run              # show what would run
//	go run ./cmd/migrate -adopt                # existing keyspace: record files as applied
//	go run ./cmd/migrate -create-keyspace      # also create the keyspace if missing
//	go run ./cmd/migrate -keyspace other -dir ./migrations
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gocql/gocql"
	"github.com/joho/godotenv"

	"github.com/StrafeChat/equinox/internal/config"
)

const trackingTable = "schema_migrations"

func main() {
	envFile := os.Getenv("ENV_FILE")
	if envFile == "" {
		envFile = ".env"
	}
	if err := godotenv.Load(envFile); err != nil && !os.IsNotExist(err) {
		log.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	dir := flag.String("dir", "./migrations", "directory holding the *.cql migration files")
	keyspace := flag.String("keyspace", cfg.Database.Scylla.Keyspace, "keyspace to migrate (default: SCYLLA_KEYSPACE)")
	hosts := flag.String("hosts", strings.Join(cfg.Database.Scylla.Hosts, ","), "comma-separated Scylla hosts (default: SCYLLA_HOSTS)")
	port := flag.Int("port", cfg.Database.Scylla.Port, "Scylla native port (default: SCYLLA_PORT)")
	createKeyspace := flag.Bool("create-keyspace", false, "create the keyspace if it does not exist")
	replication := flag.Int("replication-factor", 1, "replication factor used with -create-keyspace (SimpleStrategy)")
	dryRun := flag.Bool("dry-run", false, "list pending migrations without applying them")
	adopt := flag.Bool("adopt", false, "keyspace already migrated by hand: record every file as applied without running it (only when nothing is recorded yet)")
	flag.Parse()

	files, err := listMigrations(*dir)
	if err != nil {
		log.Fatal(err)
	}
	if len(files) == 0 {
		log.Fatalf("no *.cql files in %s", *dir)
	}

	hostList := strings.Split(*hosts, ",")
	for i := range hostList {
		hostList[i] = strings.TrimSpace(hostList[i])
	}

	if *createKeyspace && !*dryRun {
		if err := ensureKeyspace(hostList, *port, *keyspace, *replication); err != nil {
			log.Fatalf("create keyspace: %v", err)
		}
	}

	session, err := connect(hostList, *port, *keyspace)
	if err != nil {
		log.Fatalf("connect to %s/%s: %v", strings.Join(hostList, ","), *keyspace, err)
	}
	defer session.Close()

	tables, err := keyspaceTables(session, *keyspace)
	if err != nil {
		log.Fatalf("inspect keyspace: %v", err)
	}
	tracked := tables[trackingTable]
	hasOtherTables := len(tables) > 0 && !(len(tables) == 1 && tracked)

	applied := map[string]bool{}
	if tracked {
		applied, err = appliedMigrations(session)
		if err != nil {
			log.Fatalf("read %s: %v", trackingTable, err)
		}
	}

	// The dangerous case: tables exist but nothing was ever recorded. Replaying the files
	// would run their DROP/CREATE statements against live data, so refuse unless asked to
	// adopt the keyspace as-is.
	if !tracked && hasOtherTables {
		if !*adopt {
			log.Fatalf("keyspace %s already has %d tables but no %s table.\n"+
				"It was migrated outside this tool; re-running the migration files would destroy data.\n"+
				"If its schema is current, run with -adopt to record every file as applied without executing it.",
				*keyspace, len(tables), trackingTable)
		}
		if *dryRun {
			fmt.Printf("would adopt %s: record %d migrations as applied without running them\n", *keyspace, len(files))
			return
		}
		if err := ensureTrackingTable(session); err != nil {
			log.Fatalf("create %s: %v", trackingTable, err)
		}
		for _, f := range files {
			if err := recordApplied(session, filepath.Base(f)); err != nil {
				log.Fatalf("record %s: %v", filepath.Base(f), err)
			}
		}
		fmt.Printf("adopted %s: %d migrations recorded as applied (nothing executed)\n", *keyspace, len(files))
		return
	}
	if *adopt && tracked {
		fmt.Printf("%s already tracks migrations; -adopt ignored\n", *keyspace)
	}

	if !*dryRun {
		if err := ensureTrackingTable(session); err != nil {
			log.Fatalf("create %s: %v", trackingTable, err)
		}
	}

	pending := 0
	for _, f := range files {
		name := filepath.Base(f)
		if applied[name] {
			continue
		}
		pending++
		if *dryRun {
			fmt.Printf("pending  %s\n", name)
			continue
		}
		if err := applyFile(session, f); err != nil {
			log.Fatalf("%s: %v", name, err)
		}
		if err := recordApplied(session, name); err != nil {
			log.Fatalf("record %s: %v", name, err)
		}
		fmt.Printf("applied  %s\n", name)
	}
	switch {
	case pending == 0:
		fmt.Printf("up to date: %d migrations already applied to %s\n", len(applied), *keyspace)
	case *dryRun:
		fmt.Printf("%d pending (dry run, nothing applied)\n", pending)
	default:
		fmt.Printf("done: %d applied, %d already applied\n", pending, len(applied))
	}
}

func listMigrations(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.cql"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func cluster(hosts []string, port int, keyspace string) *gocql.ClusterConfig {
	c := gocql.NewCluster(hosts...)
	c.Port = port
	c.Keyspace = keyspace
	c.Consistency = gocql.Quorum
	c.Timeout = 30 * time.Second
	c.ConnectTimeout = 10 * time.Second
	return c
}

func connect(hosts []string, port int, keyspace string) (*gocql.Session, error) {
	return cluster(hosts, port, keyspace).CreateSession()
}

func ensureKeyspace(hosts []string, port int, keyspace string, rf int) error {
	s, err := connect(hosts, port, "")
	if err != nil {
		return err
	}
	defer s.Close()
	stmt := fmt.Sprintf(`CREATE KEYSPACE IF NOT EXISTS %s WITH replication = {'class': 'SimpleStrategy', 'replication_factor': %d}`, keyspace, rf)
	return s.Query(stmt).Exec()
}

// keyspaceTables lists the tables the keyspace currently has, from the schema tables.
func keyspaceTables(s *gocql.Session, keyspace string) (map[string]bool, error) {
	out := map[string]bool{}
	iter := s.Query(`SELECT table_name FROM system_schema.tables WHERE keyspace_name = ?`, keyspace).Iter()
	var name string
	for iter.Scan(&name) {
		out[name] = true
	}
	return out, iter.Close()
}

func ensureTrackingTable(s *gocql.Session) error {
	return s.Query(`CREATE TABLE IF NOT EXISTS ` + trackingTable + ` (
		name text PRIMARY KEY,
		applied_at timestamp
	)`).Exec()
}

func appliedMigrations(s *gocql.Session) (map[string]bool, error) {
	out := map[string]bool{}
	iter := s.Query(`SELECT name FROM ` + trackingTable).Iter()
	var name string
	for iter.Scan(&name) {
		out[name] = true
	}
	return out, iter.Close()
}

func recordApplied(s *gocql.Session, name string) error {
	return s.Query(`INSERT INTO `+trackingTable+` (name, applied_at) VALUES (?, ?)`, name, time.Now().UTC()).Exec()
}

// applyFile runs every statement of one migration file. The first failure stops the run
// and leaves the file unrecorded, so it is retried (from its first statement) next time -
// which is why migration files should be written to be re-runnable (IF NOT EXISTS).
func applyFile(s *gocql.Session, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for i, stmt := range splitStatements(string(raw)) {
		if err := s.Query(stmt).Exec(); err != nil {
			return fmt.Errorf("statement %d failed: %w\n%s", i+1, err, stmt)
		}
	}
	return nil
}

// splitStatements turns a .cql file into individual statements: comments (`--` to end of
// line, `/* */`) are dropped and statements end at a `;` outside single quotes.
func splitStatements(src string) []string {
	var (
		out     []string
		cur     strings.Builder
		inQuote bool
	)
	for i := 0; i < len(src); i++ {
		c := src[i]
		if !inQuote {
			if c == '-' && i+1 < len(src) && src[i+1] == '-' {
				for i < len(src) && src[i] != '\n' {
					i++
				}
				cur.WriteByte('\n')
				continue
			}
			if c == '/' && i+1 < len(src) && src[i+1] == '*' {
				end := strings.Index(src[i+2:], "*/")
				if end < 0 {
					break
				}
				i += 2 + end + 1
				continue
			}
		}
		if c == '\'' {
			inQuote = !inQuote
		}
		if c == ';' && !inQuote {
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}
