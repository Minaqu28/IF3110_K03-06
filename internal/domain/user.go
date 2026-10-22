package domain

type User struct {
	ID              int64
	Username        string
	PasswordHash    string
	IsPlatformAdmin bool
	IsActive        bool
}
