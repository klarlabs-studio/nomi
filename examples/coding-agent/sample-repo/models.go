package sample

// User is the canonical user record. Add JSON struct tags to make it
// safely encodable by encoding/json — that's the goal the coding agent
// recipe walks you through.
type User struct {
	ID    int
	Name  string
	Email string
}

// Greet returns a greeting for u using its display name.
func Greet(u User) string {
	return "Hello, " + u.Name + "!"
}
