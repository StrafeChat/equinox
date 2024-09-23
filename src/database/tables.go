package database

import "log"

func CreateTables() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS users (
		 id TEXT PRIMARY KEY,
		 email TEXT,
		 password TEXT,
		 username TEXT,
		 discriminator TEXT,
		 avatar TEXT,
		 banner TEXT,
		 bot BOOLEAN,
		 bio TEXT,
		 flags INT,
		 dob TIMESTAMP,
		 about_me TEXT,
		 accent_color TEXT,
		 locale TEXT,
		 created_at TIMESTAMP,
		 updated_at TIMESTAMP,
		 presence frozen<user_presence>
        );`,
		`CREATE TABLE IF NOT EXISTS users_by_email (
		 email TEXT PRIMARY KEY,
		 id TEXT
		);`,
		`CREATE TABLE IF NOT EXISTS users_by_username_and_discriminator (
         username TEXT,
         discriminator TEXT,
         id TEXT,
         PRIMARY KEY ((username, discriminator))
        );`,
		`CREATE TABLE IF NOT EXISTS bots (
		 user_id TEXT PRIMARY KEY,
		 owner_id TEXT,
		 public BOOLEAN,
		 description TEXT,
		 terms_of_service_url TEXT,
		 privacy_policy_url TEXT,
		 );`,
	}

	log.Println("Inserting CQL Tables.")
	for _, query := range queries {
		if err := Session.Query(query).Exec(); err != nil {
			return err
		}
	}
	return nil
}
