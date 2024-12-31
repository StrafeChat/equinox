package validation

import (
	"regexp"
	"time"
)

func IsValidEmail(email string) bool {
	re := regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
	return re.MatchString(email)
}

func DoBAbove13(dateOfBirth time.Time) bool {
	today := time.Now()
	age := today.Year() - dateOfBirth.Year()
	if today.YearDay() < dateOfBirth.YearDay() {
		age--
	}
	return age >= 13
}

func DoBAbove18(dateOfBirth time.Time) bool {
	today := time.Now()
	age := today.Year() - dateOfBirth.Year()
	if today.YearDay() < dateOfBirth.YearDay() {
		age--
	}
	return age >= 18
}
