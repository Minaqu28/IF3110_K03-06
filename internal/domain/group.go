package domain

type Group struct {
	ID      int64
	Name    string
	Members []User
}
