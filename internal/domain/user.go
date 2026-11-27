package domain

type User struct {
	ID              int64
	Username        string
	PasswordHash    string
	IsPlatformAdmin bool
	IsActive        bool
}

type Group struct {
	ID             int64
	Name           string
	LeaderID       *int64
	LeaderUsername string
	Members        []User
}

func (g Group) HasLeader(userID int64) bool {
	return g.LeaderID != nil && *g.LeaderID == userID
}

type Connection struct {
	ID      int64
	Name    string
	Host    string
	Port    int64
	DBname  string
	DBuser  string
	SSLMode string
}
