package domain

type Connection struct {
	ID      int64
	Name    string
	Host    string
	Port    int64
	Db_name string
	Db_user string
	SSLMode string
}
