package domain

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
