package database

import "log"

func CreateTyes() error {
	queries := []string{
		`CREATE TYPE IF NOT EXISTS user_presence (
         online BOOLEAN,
         status TEXT,
         custom_status TEXT
        );`,
	}

	log.Println("Inserting CQL Types.")
	for _, query := range queries {
		if err := Session.Query(query).Exec(); err != nil {
			return err
		}
	}

	return nil
}
