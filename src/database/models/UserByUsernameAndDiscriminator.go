package database

type UserByUsernameAndDiscriminator struct {
	Username      string `json:"username"`
	Discriminator string `json:"discriminator"`
	ID            string `json:"id"`
}
