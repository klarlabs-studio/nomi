package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"go.klarlabs.de/nomi/internal/storage/db"
)

func main() {
	var (
		up          = flag.Bool("up", false, "Run migrations up")
		down        = flag.Bool("down", false, "Run migrations down")
		steps       = flag.Int("steps", 0, "Number of migration steps to roll back (used with -down)")
		showVersion = flag.Bool("version", false, "Print current schema migration version and exit")
		force       = flag.Bool("force", false, "Skip confirmation prompt for down migrations")
	)
	flag.Parse()

	if *up && *down {
		log.Fatal("cannot use -up and -down together")
	}
	if *steps < 0 {
		log.Fatal("-steps must be >= 0")
	}
	if *steps > 0 && !*down {
		log.Fatal("-steps requires -down")
	}

	config := db.DefaultConfig()
	database, err := db.New(config)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer database.Close()

	if *showVersion {
		v, dirty, err := database.MigrateStatus()
		if err != nil {
			log.Fatalf("Failed to get migration status: %v", err)
		}
		if v == 0 {
			fmt.Println("Migration version: 0 (no migrations applied, dirty: false)")
			return
		}
		fmt.Printf("Migration version: %d (dirty: %v)\n", v, dirty)
		return
	}

	if *up {
		if err := database.Migrate(); err != nil {
			log.Fatalf("Failed to run migrations: %v", err)
		}
		fmt.Println("Migrations completed successfully")
		return
	}

	if *down {
		if !*force {
			fmt.Print("Down migrations can destroy data. Type 'yes' to continue: ")
			in := bufio.NewReader(os.Stdin)
			answer, err := in.ReadString('\n')
			if err != nil {
				log.Fatalf("Failed to read confirmation: %v", err)
			}
			if strings.TrimSpace(strings.ToLower(answer)) != "yes" {
				fmt.Println("Cancelled")
				return
			}
		}

		if *steps > 0 {
			if err := database.MigrateDownSteps(*steps); err != nil {
				log.Fatalf("Failed to run down migrations: %v", err)
			}
			fmt.Printf("Rolled back %d migration step(s) successfully\n", *steps)
			return
		}

		if err := database.MigrateDown(); err != nil {
			log.Fatalf("Failed to run down migrations: %v", err)
		}
		fmt.Println("Down migrations completed successfully")
		return
	}

	// Default: show status
	version, dirty, err := database.MigrateStatus()
	if err != nil {
		log.Fatalf("Failed to get migration status: %v", err)
	}

	if version == 0 {
		fmt.Println("No migrations applied yet")
	} else {
		fmt.Printf("Current migration: %d (dirty: %v)\n", version, dirty)
	}
}
