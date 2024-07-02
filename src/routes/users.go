package routes

import (
	db "go-webserver/src/database"
	"go-webserver/src/types"

	"github.com/gocql/gocql"
	"github.com/gofiber/fiber/v2"
)

func SetupUsersRoutes(app *fiber.App) {
	router := app.Group("/users")

	router.Post("/", createUser)
	router.Get("/", getUsers)
	router.Get("/:id", getUser)
	router.Delete("/:id", deleteUser)
}

func createUser(c *fiber.Ctx) error {
	var user types.User
	if err := c.BodyParser(&user); err != nil {
		return err
	}
	user.ID = gocql.TimeUUID()
	if err := saveUser(user); err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(user)
}

func saveUser(user types.User) error {
	query := db.Session.Query(`
        INSERT INTO users (id, name, email) VALUES (?, ?, ?)`,
		user.ID, user.Name, user.Email)
	return query.Exec()
}

func getUser(c *fiber.Ctx) error {
	userID, err := gocql.ParseUUID(c.Params("id"))
	if err != nil {
		return err
	}
	user, err := fetchUser(userID)
	if err != nil {
		return err
	}
	return c.JSON(user)
}

func getUsers(c *fiber.Ctx) error {
	users, err := fetchUsers()
	if err != nil {
		return err
	}
	return c.JSON(users)
}

func fetchUsers() ([]types.User, error) {
	var users []types.User
	query := db.Session.Query(`SELECT id, name, email FROM users`)
	iter := query.Iter()

	var id gocql.UUID
	var name, email string

	for iter.Scan(&id, &name, &email) {
		users = append(users, types.User{
			ID:    id,
			Name:  name,
			Email: email,
		})
	}

	if err := iter.Close(); err != nil {
		return nil, err
	}

	return users, nil
}

func fetchUser(userID gocql.UUID) (types.User, error) {
	var user types.User

	// val, errr := db.Rdb.Get(userID as).Result()
	// if errr != nil {
	// 	return user, errr
	// }
	// log.Println("key", val)

	query := db.Session.Query(`SELECT id, name, email FROM users WHERE id = ?`, userID)
	err := query.Scan(&user.ID, &user.Name, &user.Email)
	return user, err
}

func deleteUser(c *fiber.Ctx) error {
	userID, err := gocql.ParseUUID(c.Params("id"))
	if err != nil {
		return err
	}
	if err := removeUser(userID); err != nil {
		return err
	}
	return c.SendString("User deleted successfully")
}

func removeUser(userID gocql.UUID) error {
	query := db.Session.Query(`DELETE FROM users WHERE id = ?`, userID)
	return query.Exec()
}
